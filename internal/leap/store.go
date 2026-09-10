package leap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxStateSize = 256 << 10

// Provider identifies the immediate authority. Peer origin claims are never
// promoted to an independently verified publisher provenance.
type Provider struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	KeyID uint32 `json:"key_id,omitempty"`
}

type Record struct {
	Object   *Object
	Provider Provider
	Accepted time.Time
}

// State couples durable data and anti-rollback state. A committed pending
// object is retained beside the old active one until engine activation.
type State struct {
	Active    *Record
	Pending   *Record
	UTCbound  time.Time
	Executed  time.Time
	LastCheck time.Time
}

func (s State) Anchor() *Object {
	if s.Pending != nil {
		return s.Pending.Object
	}
	if s.Active != nil {
		return s.Active.Object
	}
	return nil
}

type diskRecord struct {
	Manifest Manifest  `json:"manifest"`
	Data     []byte    `json:"data"`
	Provider Provider  `json:"provider"`
	Accepted time.Time `json:"accepted_at"`
}

type diskState struct {
	Version   int         `json:"version"`
	Active    *diskRecord `json:"active,omitempty"`
	Pending   *diskRecord `json:"pending,omitempty"`
	UTCbound  time.Time   `json:"utc_bound"`
	Executed  time.Time   `json:"executed_leap,omitempty"`
	LastCheck time.Time   `json:"last_seed_check,omitzero"`
}

// Store is owned by the updater goroutine. The lock also prevents two
// processes with different control sockets from racing the same leap cache.
type Store struct {
	root *os.Root
	lock *os.File
	// fault is only set by persistence tests; it runs before the named stage.
	fault func(string) error
}

func CheckDirectory(path string) error {
	i, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !i.IsDir() || i.Mode()&os.ModeSymlink != 0 || i.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("leap: cache directory %s must be a private directory (0700)", path)
	}
	if st, ok := i.Sys().(*syscall.Stat_t); ok && os.Geteuid() != 0 && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("leap: cache directory %s is owned by another user", path)
	}
	return nil
}

func OpenStore(path string) (*Store, error) {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("leap: create cache %s: %w", path, err)
	}
	if err := CheckDirectory(path); err != nil {
		return nil, err
	}
	// Persist the cache directory's own entry too, including after a crash
	// that left the directory present but had not synced its parent.
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	err = parent.Sync()
	closeErr := parent.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return nil, fmt.Errorf("leap: sync cache parent: %w", err)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("leap: open cache %s: %w", path, err)
	}
	f, err := openNoFollow(r, ".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err == nil {
		i, e := f.Stat()
		if e != nil {
			err = e
		} else if !i.Mode().IsRegular() {
			err = errors.New("leap: cache lock must be a regular file")
		}
	}
	if err == nil {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		r.Close()
		return nil, fmt.Errorf("leap: lock cache %s: %w", path, err)
	}
	return &Store{root: r, lock: f}, nil
}

func (s *Store) Close() error { return errors.Join(s.lock.Close(), s.root.Close()) }

// InspectStore is read-only, including when the automatic cache is absent.
func InspectStore(path string) (State, error) {
	if err := CheckDirectory(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return State{}, err
	}
	defer r.Close()
	return loadState(r)
}

func (s *Store) Load() (State, error) { return loadState(s.root) }

func loadState(root *os.Root) (State, error) {
	f, err := openNoFollow(root, "state.json", os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("leap: open cache state: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return State{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxStateSize {
		return State{}, errors.New("leap: cache state must be a private, bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateSize+1))
	if err != nil {
		return State{}, err
	}
	if len(b) > maxStateSize {
		return State{}, errors.New("leap: cache state exceeds size bound")
	}
	var d diskState
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return State{}, fmt.Errorf("leap: decode cache: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return State{}, errors.New("leap: trailing cache data")
	}
	if d.Version != 1 {
		return State{}, errors.New("leap: unsupported cache version")
	}
	decode := func(d *diskRecord) (*Record, error) {
		if d == nil {
			return nil, nil
		}
		o, err := NewObject(d.Data)
		if err != nil {
			return nil, err
		}
		if o.manifest != d.Manifest || d.Accepted.IsZero() || len(d.Provider.Name) > 1024 ||
			(d.Provider.Kind != "file" && d.Provider.Kind != "nist" && d.Provider.Kind != "iers" && d.Provider.Kind != "peer") ||
			(d.Provider.Kind == "peer" && (d.Provider.KeyID == 0 || d.Provider.KeyID > 65535)) {
			return nil, errors.New("leap: cache metadata does not match its object")
		}
		return &Record{Object: o, Provider: d.Provider, Accepted: d.Accepted}, nil
	}
	a, err := decode(d.Active)
	if err != nil {
		return State{}, fmt.Errorf("leap: active cache: %w", err)
	}
	p, err := decode(d.Pending)
	if err != nil {
		return State{}, fmt.Errorf("leap: pending cache: %w", err)
	}
	if !d.UTCbound.IsZero() && (d.UTCbound.Year() < 1900 || d.UTCbound.Year() > 9999) {
		return State{}, errors.New("leap: invalid cache UTC bound")
	}
	if a != nil && p != nil {
		if err := CheckUpdate(a.Object, p.Object, p.Accepted); err != nil {
			return State{}, fmt.Errorf("leap: inconsistent pending cache: %w", err)
		}
	}
	if !d.Executed.IsZero() && (d.Executed.Year() < 1900 || d.Executed.After(d.UTCbound)) {
		return State{}, errors.New("leap: invalid execution record")
	}
	for _, r := range []*Record{a, p} {
		if r == nil {
			continue
		}
		if r.Accepted.After(d.UTCbound) {
			return State{}, errors.New("leap: acceptance after established UTC bound")
		}
		if err := CheckUpdate(nil, r.Object, r.Accepted); err != nil {
			return State{}, fmt.Errorf("leap: invalid acceptance record: %w", err)
		}
	}
	if !d.LastCheck.IsZero() && (d.LastCheck.Year() < 1900 || d.LastCheck.After(d.UTCbound)) {
		return State{}, errors.New("leap: invalid seed check date")
	}
	return State{Active: a, Pending: p, UTCbound: d.UTCbound, Executed: d.Executed, LastCheck: d.LastCheck}, nil
}

// Root.OpenFile deliberately resolves symlinks that stay inside its root,
// including with O_NOFOLLOW on some platforms. Anchor openat to the root's
// directory descriptor instead, so the kernel refuses the final symlink.
// Names here are fixed local cache entries, never paths supplied by a peer.
func openNoFollow(root *os.Root, name string, flags int, mode uint32) (*os.File, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (s *Store) Save(state State) error {
	encode := func(r *Record) *diskRecord {
		if r == nil {
			return nil
		}
		return &diskRecord{Manifest: r.Object.manifest, Data: r.Object.data, Provider: r.Provider, Accepted: r.Accepted}
	}
	b, err := json.Marshal(diskState{Version: 1, Active: encode(state.Active), Pending: encode(state.Pending), UTCbound: state.UTCbound, Executed: state.Executed, LastCheck: state.LastCheck})
	if err != nil {
		return fmt.Errorf("leap: encode cache: %w", err)
	}
	if len(b) > maxStateSize {
		return errors.New("leap: encoded cache exceeds bound")
	}
	return s.write("state.json", b, true)
}

// SeedState is the HTTPS seed's own bookkeeping: the publisher's cache
// validators for the body last evaluated and when we last asked. It lives
// in seed.json, apart from state.json, so the authority record's format and
// its rollback coupling are untouched and an older binary can still read
// the cache after a rollback. Losing it costs one unconditional download.
type SeedState struct {
	Validators  Validators
	LastAttempt time.Time
}

type diskSeed struct {
	Version      int       `json:"version"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	LastAttempt  time.Time `json:"last_attempt,omitzero"`
}

const maxSeedSize = 4 << 10

func (s *Store) LoadSeed() (SeedState, error) {
	f, err := openNoFollow(s.root, "seed.json", os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return SeedState{}, nil
	}
	if err != nil {
		return SeedState{}, fmt.Errorf("leap: open seed bookkeeping: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return SeedState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxSeedSize {
		return SeedState{}, errors.New("leap: seed bookkeeping must be a private, bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSeedSize+1))
	if err != nil {
		return SeedState{}, err
	}
	if len(b) > maxSeedSize {
		return SeedState{}, errors.New("leap: seed bookkeeping exceeds size bound")
	}
	var d diskSeed
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return SeedState{}, fmt.Errorf("leap: decode seed bookkeeping: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return SeedState{}, errors.New("leap: trailing seed bookkeeping data")
	}
	if d.Version != 1 {
		return SeedState{}, errors.New("leap: unsupported seed bookkeeping version")
	}
	v := Validators{ETag: d.ETag, LastModified: d.LastModified}
	if !v.valid() {
		return SeedState{}, errors.New("leap: invalid seed validators")
	}
	if !d.LastAttempt.IsZero() && (d.LastAttempt.Year() < 1900 || d.LastAttempt.Year() > 9999) {
		return SeedState{}, errors.New("leap: invalid seed attempt date")
	}
	return SeedState{Validators: v, LastAttempt: d.LastAttempt}, nil
}

func (s *Store) SaveSeed(state SeedState) error {
	if !state.Validators.valid() {
		return errors.New("leap: refusing to persist invalid seed validators")
	}
	b, err := json.Marshal(diskSeed{Version: 1, ETag: state.Validators.ETag, LastModified: state.Validators.LastModified, LastAttempt: state.LastAttempt})
	if err != nil {
		return fmt.Errorf("leap: encode seed bookkeeping: %w", err)
	}
	return s.write("seed.json", b, false)
}

// write replaces one cache entry atomically: private temporary file, fsync,
// rename, directory fsync. Only state.json participates in the persistence
// fault injection used by the crash tests.
func (s *Store) write(target string, b []byte, faults bool) error {
	stage := func(name string) error {
		if faults && s.fault != nil {
			return s.fault(name)
		}
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := ".state-" + hex.EncodeToString(nonce[:])
	if err := stage("create"); err != nil {
		return err
	}
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("leap: create temporary cache: %w", err)
	}
	defer s.root.Remove(name)
	defer f.Close()
	if err := stage("write"); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("leap: write cache: %w", err)
	}
	if err := stage("file_sync"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("leap: sync cache: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := stage("rename"); err != nil {
		return err
	}
	if err := s.root.Rename(name, target); err != nil {
		return fmt.Errorf("leap: replace %s: %w", target, err)
	}
	if err := stage("directory_sync"); err != nil {
		return err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("leap: sync cache directory: %w", err)
	}
	return nil
}

// CacheDir follows customized drift-file directories as well as platform
// defaults, so all daemon-owned state can live under one writable root.
func CacheDir(driftFile string) string { return filepath.Join(filepath.Dir(driftFile), "leap") }

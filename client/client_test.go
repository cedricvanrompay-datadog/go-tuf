package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/secure-systems-lab/go-securesystemslib/cjson"
	"github.com/stretchr/testify/assert"
	tuf "github.com/theupdateframework/go-tuf"
	"github.com/theupdateframework/go-tuf/data"
	"github.com/theupdateframework/go-tuf/internal/sets"
	"github.com/theupdateframework/go-tuf/pkg/keys"
	"github.com/theupdateframework/go-tuf/sign"
	"github.com/theupdateframework/go-tuf/util"
	"github.com/theupdateframework/go-tuf/verify"
	. "gopkg.in/check.v1"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) { TestingT(t) }

type ClientSuite struct {
	store       tuf.LocalStore
	repo        *tuf.Repo
	local       LocalStore
	remote      *fakeRemoteStore
	expiredTime time.Time
	keyIDs      map[string][]string
}

var _ = Suite(&ClientSuite{})

func newFakeRemoteStore() *fakeRemoteStore {
	return &fakeRemoteStore{
		meta:    make(map[string]*fakeFile),
		targets: make(map[string]*fakeFile),
	}
}

type fakeRemoteStore struct {
	meta    map[string]*fakeFile
	targets map[string]*fakeFile
}

func (f *fakeRemoteStore) GetMeta(name string) (io.ReadCloser, int64, error) {
	return f.get(name, f.meta)
}

func (f *fakeRemoteStore) GetTarget(path string) (io.ReadCloser, int64, error) {
	return f.get(path, f.targets)
}

func (f *fakeRemoteStore) get(name string, store map[string]*fakeFile) (io.ReadCloser, int64, error) {
	file, ok := store[name]
	if !ok {
		return nil, 0, ErrNotFound{name}
	}
	return file, file.size, nil
}

func newFakeFile(b []byte) *fakeFile {
	return &fakeFile{buf: bytes.NewReader(b), size: int64(len(b))}
}

type fakeFile struct {
	buf       *bytes.Reader
	bytesRead int
	size      int64
}

func (f *fakeFile) Read(p []byte) (int, error) {
	n, err := f.buf.Read(p)
	f.bytesRead += n
	return n, err
}

func (f *fakeFile) Close() error {
	f.buf.Seek(0, io.SeekStart)
	return nil
}

var targetFiles = map[string][]byte{
	"foo.txt": []byte("foo"),
	"bar.txt": []byte("bar"),
	"baz.txt": []byte("baz"),
}

func (s *ClientSuite) SetUpTest(c *C) {
	s.store = tuf.MemoryStore(nil, targetFiles)

	// create a valid repo containing foo.txt
	var err error
	s.repo, err = tuf.NewRepo(s.store)
	c.Assert(err, IsNil)
	// don't use consistent snapshots to make testing easier (consistent
	// snapshots are tested explicitly elsewhere)
	c.Assert(s.repo.Init(false), IsNil)
	s.keyIDs = map[string][]string{
		"root":      s.genKey(c, "root"),
		"targets":   s.genKey(c, "targets"),
		"snapshot":  s.genKey(c, "snapshot"),
		"timestamp": s.genKey(c, "timestamp"),
	}
	c.Assert(s.repo.AddTarget("foo.txt", nil), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)

	// create a remote store containing valid repo files
	s.remote = newFakeRemoteStore()
	s.syncRemote(c)
	for path, data := range targetFiles {
		s.remote.targets[path] = newFakeFile(data)
	}

	s.expiredTime = time.Now().Add(time.Hour)
}

func (s *ClientSuite) genKey(c *C, role string) []string {
	ids, err := s.repo.GenKey(role)
	c.Assert(err, IsNil)
	return ids
}

func (s *ClientSuite) genKeyExpired(c *C, role string) []string {
	ids, err := s.repo.GenKeyWithExpires(role, s.expiredTime)
	c.Assert(err, IsNil)
	return ids
}

// withMetaExpired sets signed.IsExpired throughout the invocation of f so that
// any metadata marked to expire at s.expiredTime will be expired (this avoids
// the need to sleep in the tests).
func (s *ClientSuite) withMetaExpired(f func()) {
	e := verify.IsExpired
	defer func() { verify.IsExpired = e }()
	verify.IsExpired = func(t time.Time) bool {
		return t.Unix() == s.expiredTime.Round(time.Second).Unix()
	}
	f()
}

func (s *ClientSuite) syncLocal(c *C) {
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)
	for k, v := range meta {
		c.Assert(s.local.SetMeta(k, v), IsNil)
	}
}

func (s *ClientSuite) syncRemote(c *C) {
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)
	for name, data := range meta {
		s.remote.meta[name] = newFakeFile(data)
	}
}

func (s *ClientSuite) addRemoteTarget(c *C, name string) {
	c.Assert(s.repo.AddTarget(name, nil), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)
}

func (s *ClientSuite) rootMeta(c *C) []byte {
	meta, err := s.repo.GetMeta()
	c.Assert(err, IsNil)
	rootMeta, ok := meta["root.json"]
	c.Assert(ok, Equals, true)
	return rootMeta
}

func (s *ClientSuite) newClient(c *C) *Client {
	s.local = MemoryLocalStore()
	client := NewClient(s.local, s.remote)
	c.Assert(client.Init(s.rootMeta(c)), IsNil)
	return client
}

func (s *ClientSuite) updatedClient(c *C) *Client {
	client := s.newClient(c)
	_, err := client.Update()
	c.Assert(err, IsNil)
	return client
}

func assertFile(c *C, file data.TargetFileMeta, name string) {
	target, ok := targetFiles[name]
	if !ok {
		c.Fatalf("unknown target %s", name)
	}

	meta, err := util.GenerateTargetFileMeta(bytes.NewReader(target), file.HashAlgorithms()...)
	c.Assert(err, IsNil)
	c.Assert(util.TargetFileMetaEqual(file, meta), IsNil)
}

func assertFiles(c *C, files data.TargetFiles, names []string) {
	c.Assert(files, HasLen, len(names))
	for _, name := range names {
		file, ok := files[name]
		if !ok {
			c.Fatalf("expected files to contain %s", name)
		}

		assertFile(c, file, name)
	}
}

func assertWrongHash(c *C, err error) {
	// just test the type of err rather using DeepEquals as it contains
	// hashes we don't necessarily need to check.
	e, ok := err.(ErrDownloadFailed)
	if !ok {
		c.Fatalf("expected err to have type ErrDownloadFailed, got %T", err)
	}
	if _, ok := e.Err.(util.ErrWrongHash); !ok {
		c.Fatalf("expected err.Err to have type util.ErrWrongHash, got %T", err)
	}
}

func (s *ClientSuite) assertErrExpired(c *C, err error, file string) {
	decodeErr, ok := err.(ErrDecodeFailed)
	if !ok {
		c.Fatalf("expected err to have type ErrDecodeFailed, got %T", err)
	}
	c.Assert(decodeErr.File, Equals, file)
	expiredErr, ok := decodeErr.Err.(verify.ErrExpired)
	if !ok {
		c.Fatalf("expected err.Err to have type signed.ErrExpired, got %T", err)
	}
	c.Assert(expiredErr.Expired.Unix(), Equals, s.expiredTime.Round(time.Second).Unix())
}

func (s *ClientSuite) TestInitAllowsExpired(c *C) {
	s.genKeyExpired(c, "targets")
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)
	client := NewClient(MemoryLocalStore(), s.remote)
	bytes, err := io.ReadAll(s.remote.meta["root.json"])
	c.Assert(err, IsNil)
	s.withMetaExpired(func() {
		c.Assert(client.Init(bytes), IsNil)
	})
}

func (s *ClientSuite) TestInit(c *C) {
	client := NewClient(MemoryLocalStore(), s.remote)
	bytes, err := io.ReadAll(s.remote.meta["root.json"])
	c.Assert(err, IsNil)
	dataSigned := &data.Signed{}
	c.Assert(json.Unmarshal(bytes, dataSigned), IsNil)
	root := &data.Root{}
	c.Assert(json.Unmarshal(dataSigned.Signed, root), IsNil)

	// check Update() returns ErrNoRootKeys when uninitialized
	_, err = client.Update()
	c.Assert(err, Equals, ErrNoRootKeys)

	// check Init() returns ErrInvalid when the root's signature is
	// invalid
	// modify root and marshal without regenerating signatures
	root.Version = root.Version + 1
	rootBytes, err := json.Marshal(root)
	c.Assert(err, IsNil)
	dataSigned.Signed = rootBytes
	dataBytes, err := json.Marshal(dataSigned)
	c.Assert(err, IsNil)
	c.Assert(client.Init(dataBytes), Equals, verify.ErrInvalid)

	// check Update() does not return ErrNoRootKeys after initialization
	c.Assert(client.Init(bytes), IsNil)
	_, err = client.Update()
	c.Assert(err, IsNil)
}

func (s *ClientSuite) TestFirstUpdate(c *C) {
	files, err := s.newClient(c).Update()
	c.Assert(err, IsNil)
	c.Assert(files, HasLen, 1)
	assertFiles(c, files, []string{"foo.txt"})
}

func (s *ClientSuite) TestMissingRemoteMetadata(c *C) {
	client := s.newClient(c)

	delete(s.remote.meta, "targets.json")
	_, err := client.Update()
	c.Assert(err, Equals, ErrMissingRemoteMetadata{"targets.json"})

	delete(s.remote.meta, "timestamp.json")
	_, err = client.Update()
	c.Assert(err, Equals, ErrMissingRemoteMetadata{"timestamp.json"})
}

func (s *ClientSuite) TestNoChangeUpdate(c *C) {
	client := s.newClient(c)
	_, err := client.Update()
	c.Assert(err, IsNil)
	_, err = client.Update()
	c.Assert(err, IsNil)
}

func (s *ClientSuite) TestNewTimestamp(c *C) {
	client := s.updatedClient(c)
	version := client.timestampVer
	c.Assert(version > 0, Equals, true)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.timestampVer > version, Equals, true)
}

func (s *ClientSuite) TestNewRoot(c *C) {
	client := s.newClient(c)

	// replace all keys
	newKeyIDs := make(map[string][]string)
	for role, ids := range s.keyIDs {
		c.Assert(len(ids) > 0, Equals, true)
		c.Assert(s.repo.RevokeKey(role, ids[0]), IsNil)
		newKeyIDs[role] = s.genKey(c, role)
	}

	// update metadata
	c.Assert(s.repo.Sign("targets.json"), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	// check update gets new root version
	c.Assert(client.getLocalMeta(), IsNil)
	version := client.rootVer
	c.Assert(version > 0, Equals, true)
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > version, Equals, true)

	// check old keys are not in db
	for _, ids := range s.keyIDs {
		c.Assert(len(ids) > 0, Equals, true)
		for _, id := range ids {
			_, err := client.db.GetVerifier(id)
			c.Assert(err, NotNil)
		}
	}

	// check new keys are in db
	for name, ids := range newKeyIDs {
		c.Assert(len(ids) > 0, Equals, true)
		for _, id := range ids {
			verifier, err := client.db.GetVerifier(id)
			c.Assert(err, IsNil)
			c.Assert(verifier.MarshalPublicKey().IDs(), DeepEquals, ids)
		}
		role := client.db.GetRole(name)
		c.Assert(role, NotNil)
		c.Assert(role.KeyIDs, DeepEquals, sets.StringSliceToSet(ids))
	}
}

// startTUFRepoServer starts a HTTP server to serve a TUF Repo.
func startTUFRepoServer(baseDir string, relPath string) (net.Listener, error) {
	serverDir := filepath.Join(baseDir, relPath)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(l, http.FileServer(http.Dir(serverDir)))
	return l, err
}

// newClientWithMeta creates new client and sets the root metadata for it.
func newClientWithMeta(baseDir string, relPath string, serverAddr string) (*Client, error) {
	initialStateDir := filepath.Join(baseDir, relPath)
	opts := &HTTPRemoteOptions{
		MetadataPath: "metadata",
		TargetsPath:  "targets",
	}

	remote, err := HTTPRemoteStore(fmt.Sprintf("http://%s/", serverAddr), opts, nil)
	if err != nil {
		return nil, err
	}
	c := NewClient(MemoryLocalStore(), remote)
	for _, m := range []string{"root.json", "snapshot.json", "timestamp.json", "targets.json"} {
		if _, err := os.Stat(initialStateDir + "/" + m); err == nil {
			metadataJSON, err := ioutil.ReadFile(initialStateDir + "/" + m)
			if err != nil {
				return nil, err
			}
			c.local.SetMeta(m, metadataJSON)
		}
	}
	return c, nil
}

func initRootTest(c *C, baseDir string) (*Client, func() error) {
	l, err := startTUFRepoServer(baseDir, "server")
	c.Assert(err, IsNil)
	tufClient, err := newClientWithMeta(baseDir, "client/metadata/current", l.Addr().String())
	c.Assert(err, IsNil)
	return tufClient, l.Close
}

func (s *ClientSuite) TestUpdateRoots(c *C) {
	var tests = []struct {
		fixturePath      string
		expectedError    error
		expectedVersions map[string]int64
	}{
		// Succeeds when there is no root update.
		{"testdata/Published1Time", nil, map[string]int64{"root": 1, "timestamp": 1, "snapshot": 1, "targets": 1}},
		// Succeeds when client only has root.json
		{"testdata/Published1Time_client_root_only", nil, map[string]int64{"root": 1, "timestamp": 1, "snapshot": 1, "targets": 1}},
		// Succeeds updating root from version 1 to version 2.
		{"testdata/Published2Times_keyrotated", nil, map[string]int64{"root": 2, "timestamp": 1, "snapshot": 1, "targets": 1}},
		// Succeeds updating root from version 1 to version 2 when the client's initial root version is expired.
		{"testdata/Published2Times_keyrotated_initialrootexpired", nil, map[string]int64{"root": 2, "timestamp": 1, "snapshot": 1, "targets": 1}},
		// Succeeds updating root from version 1 to version 3 when versions 1 and 2 are expired.
		{"testdata/Published3Times_keyrotated_initialrootsexpired", nil, map[string]int64{"root": 3, "timestamp": 1, "snapshot": 1, "targets": 1}},
		// Succeeds updating root from version 2 to version 3.
		{"testdata/Published3Times_keyrotated_initialrootsexpired_clientversionis2", nil, map[string]int64{"root": 3, "timestamp": 1, "snapshot": 1, "targets": 1}},
		// Fails updating root from version 1 to version 3 when versions 1 and 3 are expired but version 2 is not expired.
		{"testdata/Published3Times_keyrotated_latestrootexpired", ErrDecodeFailed{File: "root.json", Err: verify.ErrExpired{}}, map[string]int64{"root": 2, "timestamp": 1, "snapshot": 1, "targets": 1}},
		// Fails updating root from version 1 to version 2 when old root 1 did not sign off on it (nth root didn't sign off n+1).
		{"testdata/Published2Times_keyrotated_invalidOldRootSignature", errors.New("tuf: signature verification failed"), map[string]int64{}},
		// Fails updating root from version 1 to version 2 when the new root 2 did not sign itself (n+1th root didn't sign off n+1)
		{"testdata/Published2Times_keyrotated_invalidNewRootSignature", errors.New("tuf: signature verification failed"), map[string]int64{}},
		// Fails updating root to 2.root.json when the value of the version field inside it is 1 (rollback attack prevention).
		{"testdata/Published1Time_backwardRootVersion", verify.ErrWrongVersion(verify.ErrWrongVersion{Given: 1, Expected: 2}), map[string]int64{}},
		// Fails updating root to 2.root.json when the value of the version field inside it is 3 (rollforward attack prevention).
		{"testdata/Published3Times_keyrotated_forwardRootVersion", verify.ErrWrongVersion(verify.ErrWrongVersion{Given: 3, Expected: 2}), map[string]int64{}},
		// Fails updating when there is no local trusted root.
		{"testdata/Published1Time_client_no_root", errors.New("tuf: no root keys found in local meta store"), map[string]int64{}},

		// snapshot role key rotation increase the snapshot and timestamp.
		{"testdata/Published2Times_snapshot_keyrotated", nil, map[string]int64{"root": 2, "timestamp": 2, "snapshot": 2, "targets": 1}},
		// targets role key rotation increase the snapshot, timestamp, and targets.
		{"testdata/Published2Times_targets_keyrotated", nil, map[string]int64{"root": 2, "timestamp": 2, "snapshot": 2, "targets": 2}},
		// timestamp role key rotation increase the timestamp.
		{"testdata/Published2Times_timestamp_keyrotated", nil, map[string]int64{"root": 2, "timestamp": 2, "snapshot": 1, "targets": 1}},
		//root file size > defaultRootDownloadLimit
		{"testdata/Published2Times_roottoolarge", ErrMetaTooLarge{Name: "2.root.json", Size: defaultRootDownloadLimit + 1, MaxSize: defaultRootDownloadLimit}, map[string]int64{}},
	}

	for _, test := range tests {
		tufClient, closer := initRootTest(c, test.fixturePath)
		_, err := tufClient.Update()
		if test.expectedError == nil {
			c.Assert(err, IsNil)
			// Check if the root.json is being saved in non-volatile storage.
			tufClient.getLocalMeta()
			versionMethods := map[string]int64{"root": tufClient.rootVer,
				"timestamp": tufClient.timestampVer,
				"snapshot":  tufClient.snapshotVer,
				"targets":   tufClient.targetsVer}
			for m, v := range test.expectedVersions {
				assert.Equal(c, v, versionMethods[m])
			}
		} else {
			// For backward compatibility, the update root returns
			// ErrDecodeFailed that wraps the verify.ErrExpired.
			if _, ok := test.expectedError.(ErrDecodeFailed); ok {
				decodeErr, ok := err.(ErrDecodeFailed)
				c.Assert(ok, Equals, true)
				c.Assert(decodeErr.File, Equals, "root.json")
				_, ok = decodeErr.Err.(verify.ErrExpired)
				c.Assert(ok, Equals, true)
			} else {
				assert.Equal(c, test.expectedError, err)
			}
		}
		closer()
	}
}

func (s *ClientSuite) TestFastForwardAttackRecovery(c *C) {
	var tests = []struct {
		fixturePath       string
		expectMetaDeleted map[string]bool
	}{
		// Each of the following test cases each has a two sets of TUF metadata:
		// (1) client's initial, and (2) server's current.
		// The naming format is PublishedTwiceMultiKeysadd_X_revoke_Y_threshold_Z_ROLE
		// The client includes TUF metadata before key rotation for TUF ROLE with X keys.
		// The server includes updated TUF metadata after key rotation. The
		// rotation involves revoking Y keys from the initial keys.
		// For each test, the TUF client's will be initialized to the client files.
		// The test checks whether the client is  able to update itself properly.

		// Fast-forward recovery is not needed if less than threshold keys are revoked.
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_2_threshold_4_root",
			map[string]bool{"root.json": false, "timestamp.json": false, "snapshot.json": false, "targets.json": false}},
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_2_threshold_4_snapshot",
			map[string]bool{"root.json": false, "timestamp.json": false, "snapshot.json": false, "targets.json": false}},
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_2_threshold_4_targets",
			map[string]bool{"root.json": false, "timestamp.json": false, "snapshot.json": false, "targets.json": false}},
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_2_threshold_4_timestamp",
			map[string]bool{"root.json": false, "timestamp.json": false, "snapshot.json": false, "targets.json": false}},

		// Fast-forward recovery not needed if root keys are revoked, even when the threshold number of root keys are revoked.
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_4_threshold_4_root",
			map[string]bool{"root.json": false, "timestamp.json": false, "snapshot.json": false, "targets.json": false}},

		// Delete snapshot and timestamp metadata if a threshold number of snapshot keys are revoked.
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_4_threshold_4_snapshot",
			map[string]bool{"root.json": false, "timestamp.json": true, "snapshot.json": true, "targets.json": false}},
		// Delete targets and snapshot metadata if a threshold number of targets keys are revoked.
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_4_threshold_4_targets",
			map[string]bool{"root.json": false, "timestamp.json": false, "snapshot.json": true, "targets.json": true}},
		// Delete timestamp metadata if a threshold number of timestamp keys are revoked.
		{"testdata/PublishedTwiceMultiKeysadd_9_revoke_4_threshold_4_timestamp",
			map[string]bool{"root.json": false, "timestamp.json": true, "snapshot.json": false, "targets.json": false}},
	}
	for _, test := range tests {
		tufClient, closer := initRootTest(c, test.fixturePath)
		c.Assert(tufClient.UpdateRoots(), IsNil)
		m, err := tufClient.local.GetMeta()
		c.Assert(err, IsNil)
		for md, deleted := range test.expectMetaDeleted {
			if deleted {
				if _, ok := m[md]; ok {
					c.Fatalf("Metadata %s is not deleted!", md)
				}
			} else {
				if _, ok := m[md]; !ok {
					c.Fatalf("Metadata %s deleted!", md)
				}
			}
		}
		closer()
	}

}

func (s *ClientSuite) TestUpdateRace(c *C) {
	// Tests race condition for the client update. You need to run the test with -race flag:
	// go test -race
	for i := 0; i < 2; i++ {
		go func() {
			c := NewClient(MemoryLocalStore(), newFakeRemoteStore())
			c.Update()
		}()
	}
}

func (s *ClientSuite) TestNewTargets(c *C) {
	client := s.newClient(c)
	files, err := client.Update()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"foo.txt"})

	s.addRemoteTarget(c, "bar.txt")
	s.addRemoteTarget(c, "baz.txt")

	files, err = client.Update()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"bar.txt", "baz.txt"})

	// Adding the same exact file should not lead to an update
	s.addRemoteTarget(c, "bar.txt")
	files, err = client.Update()
	c.Assert(err, IsNil)
	c.Assert(files, HasLen, 0)
}

func (s *ClientSuite) TestNewTimestampKey(c *C) {
	client := s.newClient(c)

	// replace key
	oldIDs := s.keyIDs["timestamp"]
	c.Assert(s.repo.RevokeKey("timestamp", oldIDs[0]), IsNil)
	newIDs := s.genKey(c, "timestamp")

	// generate new snapshot (because root has changed) and timestamp
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	// check update gets new root and timestamp
	c.Assert(client.getLocalMeta(), IsNil)
	rootVer := client.rootVer
	timestampVer := client.timestampVer
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > rootVer, Equals, true)
	c.Assert(client.timestampVer > timestampVer, Equals, true)

	// check key has been replaced in db
	for _, oldID := range oldIDs {
		_, err := client.db.GetVerifier(oldID)
		c.Assert(err, NotNil)
	}
	for _, newID := range newIDs {
		verifier, err := client.db.GetVerifier(newID)
		c.Assert(err, IsNil)
		c.Assert(verifier.MarshalPublicKey().IDs(), DeepEquals, newIDs)
	}
	role := client.db.GetRole("timestamp")
	c.Assert(role, NotNil)
	c.Assert(role.KeyIDs, DeepEquals, sets.StringSliceToSet(newIDs))
}

func (s *ClientSuite) TestNewSnapshotKey(c *C) {
	client := s.newClient(c)

	// replace key
	oldIDs := s.keyIDs["snapshot"]
	c.Assert(s.repo.RevokeKey("snapshot", oldIDs[0]), IsNil)
	newIDs := s.genKey(c, "snapshot")

	// generate new snapshot and timestamp
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	// check update gets new root, snapshot and timestamp
	c.Assert(client.getLocalMeta(), IsNil)
	rootVer := client.rootVer
	snapshotVer := client.snapshotVer
	timestampVer := client.timestampVer
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > rootVer, Equals, true)
	c.Assert(client.snapshotVer > snapshotVer, Equals, true)
	c.Assert(client.timestampVer > timestampVer, Equals, true)

	// check key has been replaced in db
	for _, oldID := range oldIDs {
		_, err := client.db.GetVerifier(oldID)
		c.Assert(err, NotNil)
	}
	for _, newID := range newIDs {
		verifier, err := client.db.GetVerifier(newID)
		c.Assert(err, IsNil)
		c.Assert(verifier.MarshalPublicKey().IDs(), DeepEquals, newIDs)
	}
	role := client.db.GetRole("snapshot")
	c.Assert(role, NotNil)
	c.Assert(role.KeyIDs, DeepEquals, sets.StringSliceToSet(newIDs))
}

func (s *ClientSuite) TestNewTargetsKey(c *C) {
	client := s.newClient(c)

	// replace key
	oldIDs := s.keyIDs["targets"]
	c.Assert(s.repo.RevokeKey("targets", oldIDs[0]), IsNil)
	newIDs := s.genKey(c, "targets")

	// re-sign targets and generate new snapshot and timestamp
	c.Assert(s.repo.Sign("targets.json"), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	// check update gets new metadata
	c.Assert(client.getLocalMeta(), IsNil)
	rootVer := client.rootVer
	targetsVer := client.targetsVer
	snapshotVer := client.snapshotVer
	timestampVer := client.timestampVer
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.rootVer > rootVer, Equals, true)
	c.Assert(client.targetsVer > targetsVer, Equals, true)
	c.Assert(client.snapshotVer > snapshotVer, Equals, true)
	c.Assert(client.timestampVer > timestampVer, Equals, true)

	// check key has been replaced in db
	for _, oldID := range oldIDs {
		_, err := client.db.GetVerifier(oldID)
		c.Assert(err, NotNil)
	}
	for _, newID := range newIDs {
		verifier, err := client.db.GetVerifier(newID)
		c.Assert(err, IsNil)
		c.Assert(verifier.MarshalPublicKey().IDs(), DeepEquals, newIDs)
	}
	role := client.db.GetRole("targets")
	c.Assert(role, NotNil)
	c.Assert(role.KeyIDs, DeepEquals, sets.StringSliceToSet(newIDs))
}

func (s *ClientSuite) TestOfflineSignatureFlow(c *C) {
	client := s.newClient(c)

	// replace key
	oldIDs := s.keyIDs["targets"]
	c.Assert(s.repo.RevokeKey("targets", oldIDs[0]), IsNil)
	_ = s.genKey(c, "targets")

	// re-sign targets using offline flow and generate new snapshot and timestamp
	payload, err := s.repo.Payload("targets.json")
	c.Assert(err, IsNil)
	signed := data.Signed{Signed: payload}
	_, err = s.repo.SignPayload("targets", &signed)
	c.Assert(err, IsNil)
	for _, sig := range signed.Signatures {
		// This method checks that the signature verifies!
		err = s.repo.AddOrUpdateSignature("targets.json", sig)
		c.Assert(err, IsNil)
	}
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	// check update gets new metadata
	c.Assert(client.getLocalMeta(), IsNil)
	_, err = client.Update()
	c.Assert(err, IsNil)
}

func (s *ClientSuite) TestLocalExpired(c *C) {
	client := s.newClient(c)

	// locally expired timestamp.json is ok
	version := client.timestampVer
	c.Assert(s.repo.TimestampWithExpires(s.expiredTime), IsNil)
	s.syncLocal(c)
	s.withMetaExpired(func() {
		c.Assert(client.getLocalMeta(), IsNil)
		c.Assert(client.timestampVer > version, Equals, true)
	})

	// locally expired snapshot.json is ok
	version = client.snapshotVer
	c.Assert(s.repo.SnapshotWithExpires(s.expiredTime), IsNil)
	s.syncLocal(c)
	s.withMetaExpired(func() {
		c.Assert(client.getLocalMeta(), IsNil)
		c.Assert(client.snapshotVer > version, Equals, true)
	})

	// locally expired targets.json is ok
	version = client.targetsVer
	c.Assert(s.repo.AddTargetWithExpires("foo.txt", nil, s.expiredTime), IsNil)
	s.syncLocal(c)
	s.withMetaExpired(func() {
		c.Assert(client.getLocalMeta(), IsNil)
		c.Assert(client.targetsVer > version, Equals, true)
	})

	// locally expired root.json is not ok
	version = client.rootVer
	s.genKeyExpired(c, "targets")
	s.syncLocal(c)
	s.withMetaExpired(func() {
		err := client.getLocalMeta()
		if _, ok := err.(verify.ErrExpired); !ok {
			c.Fatalf("expected err to have type signed.ErrExpired, got %T", err)
		}
		c.Assert(client.rootVer, Equals, version)
	})
}

func (s *ClientSuite) TestTimestampTooLarge(c *C) {
	s.remote.meta["timestamp.json"] = newFakeFile(make([]byte, defaultTimestampDownloadLimit+1))
	_, err := s.newClient(c).Update()
	c.Assert(err, Equals, ErrMetaTooLarge{"timestamp.json", defaultTimestampDownloadLimit + 1, defaultTimestampDownloadLimit})
}

func (s *ClientSuite) TestUpdateLocalRootExpired(c *C) {
	client := s.newClient(c)

	// add soon to expire root.json to local storage
	s.genKeyExpired(c, "timestamp")
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncLocal(c)

	// add far expiring root.json to remote storage
	s.genKey(c, "timestamp")
	s.addRemoteTarget(c, "bar.txt")
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	const expectedRootVersion = int64(3)

	// check the update downloads the non expired remote root.json and
	// restarts itself, thus successfully updating
	s.withMetaExpired(func() {
		err := client.getLocalMeta()
		if _, ok := err.(verify.ErrExpired); !ok {
			c.Fatalf("expected err to have type signed.ErrExpired, got %T", err)
		}
		_, err = client.Update()
		c.Assert(err, IsNil)
		c.Assert(client.rootVer, Equals, expectedRootVersion)
	})
}

func (s *ClientSuite) TestUpdateRemoteExpired(c *C) {
	client := s.updatedClient(c)

	// expired remote metadata should always be rejected
	c.Assert(s.repo.TimestampWithExpires(s.expiredTime), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "timestamp.json")
	})

	c.Assert(s.repo.SnapshotWithExpires(s.expiredTime), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "snapshot.json")
	})

	c.Assert(s.repo.AddTargetWithExpires("bar.txt", nil, s.expiredTime), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "targets.json")
	})

	s.genKeyExpired(c, "timestamp")
	c.Assert(s.repo.RemoveTarget("bar.txt"), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)
	s.withMetaExpired(func() {
		_, err := client.Update()
		s.assertErrExpired(c, err, "root.json")
	})
}

func (s *ClientSuite) TestUpdateLocalRootExpiredKeyChange(c *C) {
	client := s.newClient(c)

	// add soon to expire root.json to local storage
	s.genKeyExpired(c, "timestamp")
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncLocal(c)

	// replace all keys
	newKeyIDs := make(map[string][]string)
	for role, ids := range s.keyIDs {
		if role != "snapshot" && role != "timestamp" && role != "targets" {
			c.Assert(s.repo.RevokeKey(role, ids[0]), IsNil)
			newKeyIDs[role] = s.genKey(c, role)
		}
	}

	// update metadata
	c.Assert(s.repo.Sign("targets.json"), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	// check the update downloads the non expired remote root.json and
	// restarts itself, thus successfully updating
	s.withMetaExpired(func() {
		err := client.getLocalMeta()
		c.Assert(err, FitsTypeOf, verify.ErrExpired{})

		_, err = client.Update()
		c.Assert(err, IsNil)
	})
}

func (s *ClientSuite) TestUpdateMixAndMatchAttack(c *C) {
	// generate metadata with an explicit expires so we can make predictable changes
	expires := time.Now().Add(time.Hour)
	c.Assert(s.repo.AddTargetWithExpires("foo.txt", nil, expires), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	client := s.updatedClient(c)

	// grab the remote targets.json
	oldTargets, ok := s.remote.meta["targets.json"]
	if !ok {
		c.Fatal("missing remote targets.json")
	}

	// generate new remote metadata, but replace targets.json with the old one
	c.Assert(s.repo.AddTargetWithExpires("bar.txt", nil, expires), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	newTargets, ok := s.remote.meta["targets.json"]
	if !ok {
		c.Fatal("missing remote targets.json")
	}
	s.remote.meta["targets.json"] = oldTargets

	// check update returns ErrWrongSize for targets.json
	_, err := client.Update()
	c.Assert(err, DeepEquals, ErrWrongSize{"targets.json", oldTargets.size, newTargets.size})

	// do the same but keep the size the same
	c.Assert(s.repo.RemoveTargetWithExpires("foo.txt", expires), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	s.remote.meta["targets.json"] = oldTargets

	// check update returns ErrWrongHash
	_, err = client.Update()
	assertWrongHash(c, err)
}

func (s *ClientSuite) TestUpdateReplayAttack(c *C) {
	client := s.updatedClient(c)

	// grab the remote timestamp.json
	oldTimestamp, ok := s.remote.meta["timestamp.json"]
	if !ok {
		c.Fatal("missing remote timestamp.json")
	}

	// generate a new timestamp and sync with the client
	version := client.timestampVer
	c.Assert(version > 0, Equals, true)
	c.Assert(s.repo.Timestamp(), IsNil)
	s.syncRemote(c)
	_, err := client.Update()
	c.Assert(err, IsNil)
	c.Assert(client.timestampVer > version, Equals, true)

	// replace remote timestamp.json with the old one
	s.remote.meta["timestamp.json"] = oldTimestamp

	// check update returns ErrLowVersion
	_, err = client.Update()
	c.Assert(err, DeepEquals, ErrDecodeFailed{
		File: "timestamp.json",
		Err: verify.ErrLowVersion{
			Actual:  version,
			Current: client.timestampVer,
		},
	})
}

func (s *ClientSuite) TestUpdateTamperedTargets(c *C) {
	client := s.newClient(c)

	// get local targets.json
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)
	targetsJSON, ok := meta["targets.json"]
	if !ok {
		c.Fatal("missing targets.json")
	}

	type signedTargets struct {
		Signed     data.Targets     `json:"signed"`
		Signatures []data.Signature `json:"signatures"`
	}
	targets := &signedTargets{}
	c.Assert(json.Unmarshal(targetsJSON, targets), IsNil)

	// update remote targets.json to have different content but same size
	targets.Signed.Type = "xxxxxxx"
	tamperedJSON, err := json.Marshal(targets)
	c.Assert(err, IsNil)
	s.store.SetMeta("targets.json", tamperedJSON)
	s.store.Commit(false, nil, nil)
	s.syncRemote(c)
	_, err = client.Update()
	assertWrongHash(c, err)

	// update remote targets.json to have the wrong size
	targets.Signed.Type = "xxx"
	tamperedJSON, err = json.Marshal(targets)
	c.Assert(err, IsNil)
	s.store.SetMeta("targets.json", tamperedJSON)
	s.store.Commit(false, nil, nil)
	s.syncRemote(c)
	_, err = client.Update()
	c.Assert(err, DeepEquals, ErrWrongSize{"targets.json", int64(len(tamperedJSON)), int64(len(targetsJSON))})
}

func (s *ClientSuite) TestUpdateHTTP(c *C) {
	tmp := c.MkDir()

	// start file server
	addr, cleanup := startFileServer(c, tmp)
	defer cleanup()

	for _, consistentSnapshot := range []bool{false, true} {
		dir := fmt.Sprintf("consistent-snapshot-%t", consistentSnapshot)

		// generate repository
		repo := generateRepoFS(c, filepath.Join(tmp, dir), targetFiles, consistentSnapshot)

		// initialize a client
		remote, err := HTTPRemoteStore(fmt.Sprintf("http://%s/%s/repository", addr, dir), nil, nil)
		c.Assert(err, IsNil)
		client := NewClient(MemoryLocalStore(), remote)
		rootMeta, err := repo.SignedMeta("root.json")
		c.Assert(err, IsNil)
		rootJsonBytes, err := json.Marshal(rootMeta)
		c.Assert(err, IsNil)
		c.Assert(client.Init(rootJsonBytes), IsNil)

		// check update is ok
		targets, err := client.Update()
		c.Assert(err, IsNil)
		assertFiles(c, targets, []string{"foo.txt", "bar.txt", "baz.txt"})

		// check can download files
		for name, data := range targetFiles {
			var dest testDestination
			c.Assert(client.Download(name, &dest), IsNil)
			c.Assert(dest.deleted, Equals, false)
			c.Assert(dest.String(), Equals, string(data))
		}
	}
}

type testDestination struct {
	bytes.Buffer
	deleted bool
}

func (t *testDestination) Delete() error {
	t.deleted = true
	return nil
}

func (s *ClientSuite) TestDownloadUnknownTarget(c *C) {
	client := s.updatedClient(c)
	var dest testDestination
	c.Assert(client.Download("nonexistent", &dest), Equals, ErrUnknownTarget{Name: "nonexistent", SnapshotVersion: 1})
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadNoExist(c *C) {
	client := s.updatedClient(c)
	delete(s.remote.targets, "foo.txt")
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), Equals, ErrNotFound{"foo.txt"})
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadOK(c *C) {
	client := s.updatedClient(c)
	// the filename is normalized if necessary
	for _, name := range []string{"/foo.txt", "foo.txt"} {
		var dest testDestination
		c.Assert(client.Download(name, &dest), IsNil)
		c.Assert(dest.deleted, Equals, false)
		c.Assert(dest.String(), Equals, "foo")
	}
}

func (s *ClientSuite) TestDownloadWrongSize(c *C) {
	client := s.updatedClient(c)
	remoteFile := &fakeFile{buf: bytes.NewReader([]byte("wrong-size")), size: 10}
	s.remote.targets["foo.txt"] = remoteFile
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), DeepEquals, ErrWrongSize{"foo.txt", 10, 3})
	c.Assert(remoteFile.bytesRead, Equals, 0)
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadTargetTooLong(c *C) {
	client := s.updatedClient(c)
	remoteFile := s.remote.targets["foo.txt"]
	remoteFile.buf = bytes.NewReader([]byte("foo-ooo"))
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), IsNil)
	c.Assert(remoteFile.bytesRead, Equals, 3)
	c.Assert(dest.deleted, Equals, false)
	c.Assert(dest.String(), Equals, "foo")
}

func (s *ClientSuite) TestDownloadTargetTooShort(c *C) {
	client := s.updatedClient(c)
	remoteFile := s.remote.targets["foo.txt"]
	remoteFile.buf = bytes.NewReader([]byte("fo"))
	var dest testDestination
	c.Assert(client.Download("foo.txt", &dest), DeepEquals, ErrWrongSize{"foo.txt", 2, 3})
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestDownloadTargetCorruptData(c *C) {
	client := s.updatedClient(c)
	remoteFile := s.remote.targets["foo.txt"]
	remoteFile.buf = bytes.NewReader([]byte("corrupt"))
	var dest testDestination
	assertWrongHash(c, client.Download("foo.txt", &dest))
	c.Assert(dest.deleted, Equals, true)
}

func (s *ClientSuite) TestAvailableTargets(c *C) {
	client := s.updatedClient(c)
	files, err := client.Targets()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"foo.txt"})

	s.addRemoteTarget(c, "bar.txt")
	s.addRemoteTarget(c, "baz.txt")
	_, err = client.Update()
	c.Assert(err, IsNil)
	files, err = client.Targets()
	c.Assert(err, IsNil)
	assertFiles(c, files, []string{"foo.txt", "bar.txt", "baz.txt"})
}

func (s *ClientSuite) TestAvailableTarget(c *C) {
	client := s.updatedClient(c)

	target, err := client.Target("foo.txt")
	c.Assert(err, IsNil)
	assertFile(c, target, "foo.txt")

	target, err = client.Target("/foo.txt")
	c.Assert(err, IsNil)
	assertFile(c, target, "foo.txt")

	_, err = client.Target("bar.txt")
	c.Assert(err, Equals, ErrNotFound{"bar.txt"})

	_, err = client.Target("/bar.txt")
	c.Assert(err, Equals, ErrNotFound{"/bar.txt"})
}

func (s *ClientSuite) TestUnknownKeyIDs(c *C) {
	// get local root.json
	meta, err := s.store.GetMeta()
	c.Assert(err, IsNil)

	rootJSON, ok := meta["root.json"]
	c.Assert(ok, Equals, true)

	var root struct {
		Signed     data.Root        `json:"signed"`
		Signatures []data.Signature `json:"signatures"`
	}
	c.Assert(json.Unmarshal(rootJSON, &root), IsNil)

	// update remote root.json to add a new key with an unknown id
	signer, err := keys.GenerateEd25519Key()
	c.Assert(err, IsNil)

	root.Signed.Keys["unknown-key-id"] = signer.PublicData()

	// re-sign the root metadata, then commit it back into the store.
	signingKeys, err := s.store.GetSigners("root")
	c.Assert(err, IsNil)

	signedRoot, err := sign.Marshal(root.Signed, signingKeys...)
	c.Assert(err, IsNil)

	rootJSON, err = cjson.EncodeCanonical(signedRoot)
	c.Assert(err, IsNil)

	s.store.SetMeta("root.json", rootJSON)
	s.store.Commit(false, nil, nil)
	s.syncRemote(c)

	// FIXME(TUF-0.9) We need this for now because the client still uses
	// the TUF-0.9 update workflow, where we decide to update the root
	// metadata when we observe a new root through the snapshot.
	repo, err := tuf.NewRepo(s.store)
	c.Assert(err, IsNil)
	c.Assert(repo.Snapshot(), IsNil)
	c.Assert(repo.Timestamp(), IsNil)
	c.Assert(repo.Commit(), IsNil)
	s.syncRemote(c)

	// Make sure the client can update with the unknown keyid.
	client := s.newClient(c)
	_, err = client.Update()
	c.Assert(err, IsNil)
}

func generateRepoFS(c *C, dir string, files map[string][]byte, consistentSnapshot bool) *tuf.Repo {
	repo, err := tuf.NewRepo(tuf.FileSystemStore(dir, nil))
	c.Assert(err, IsNil)
	if !consistentSnapshot {
		c.Assert(repo.Init(false), IsNil)
	}
	for _, role := range []string{"root", "snapshot", "targets", "timestamp"} {
		_, err := repo.GenKey(role)
		c.Assert(err, IsNil)
	}
	for file, data := range files {
		path := filepath.Join(dir, "staged", "targets", file)
		c.Assert(os.MkdirAll(filepath.Dir(path), 0755), IsNil)
		c.Assert(ioutil.WriteFile(path, data, 0644), IsNil)
		c.Assert(repo.AddTarget(file, nil), IsNil)
	}
	c.Assert(repo.Snapshot(), IsNil)
	c.Assert(repo.Timestamp(), IsNil)
	c.Assert(repo.Commit(), IsNil)
	return repo
}

func (s *ClientSuite) TestVerifyDigest(c *C) {
	digest := "sha256:bc11b176a293bb341a0f2d0d226f52e7fcebd186a7c4dfca5fc64f305f06b94c"
	hash := "bc11b176a293bb341a0f2d0d226f52e7fcebd186a7c4dfca5fc64f305f06b94c"
	size := int64(42)

	c.Assert(s.repo.AddTargetsWithDigest(hash, "sha256", size, digest, nil), IsNil)
	c.Assert(s.repo.Snapshot(), IsNil)
	c.Assert(s.repo.Timestamp(), IsNil)
	c.Assert(s.repo.Commit(), IsNil)
	s.syncRemote(c)

	client := s.newClient(c)
	_, err := client.Update()
	c.Assert(err, IsNil)

	c.Assert(client.VerifyDigest(hash, "sha256", size, digest), IsNil)
}

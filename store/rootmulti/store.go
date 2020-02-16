package rootmulti

import (
	"bytes"
	"crypto/sha1" // nolint: gosec
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"

	"github.com/pkg/errors"
	tmiavl "github.com/tendermint/iavl"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/merkle"
	"github.com/tendermint/tendermint/crypto/tmhash"
	dbm "github.com/tendermint/tm-db"

	"github.com/cosmos/cosmos-sdk/store/cachemulti"
	"github.com/cosmos/cosmos-sdk/store/dbadapter"
	"github.com/cosmos/cosmos-sdk/store/iavl"
	"github.com/cosmos/cosmos-sdk/store/tracekv"
	"github.com/cosmos/cosmos-sdk/store/transient"
	"github.com/cosmos/cosmos-sdk/store/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

const (
	latestVersionKey = "s/latest"
	commitInfoKeyFmt = "s/%d" // s/<version>
)

// Store is composed of many CommitStores. Name contrasts with
// cacheMultiStore which is for cache-wrapping other MultiStores. It implements
// the CommitMultiStore interface.
type Store struct {
	db           dbm.DB
	lastCommitID types.CommitID
	pruningOpts  types.PruningOptions
	storesParams map[types.StoreKey]storeParams
	stores       map[types.StoreKey]types.CommitKVStore
	keysByName   map[string]types.StoreKey
	lazyLoading  bool

	traceWriter  io.Writer
	traceContext types.TraceContext

	interBlockCache types.MultiStorePersistentCache
}

var _ types.CommitMultiStore = (*Store)(nil)
var _ types.Queryable = (*Store)(nil)

// NewStore returns a reference to a new Store object with the provided DB. The
// store will be created with a PruneNothing pruning strategy by default. After
// a store is created, KVStores must be mounted and finally LoadLatestVersion or
// LoadVersion must be called.
func NewStore(db dbm.DB) *Store {
	return &Store{
		db:           db,
		pruningOpts:  types.PruneNothing,
		storesParams: make(map[types.StoreKey]storeParams),
		stores:       make(map[types.StoreKey]types.CommitKVStore),
		keysByName:   make(map[string]types.StoreKey),
	}
}

// SetPruning sets the pruning strategy on the root store and all the sub-stores.
// Note, calling SetPruning on the root store prior to LoadVersion or
// LoadLatestVersion performs a no-op as the stores aren't mounted yet.
//
// TODO: Consider removing this API altogether on sub-stores as a pruning
// strategy should only be provided on initialization.
func (rs *Store) SetPruning(pruningOpts types.PruningOptions) {
	rs.pruningOpts = pruningOpts
	for _, substore := range rs.stores {
		substore.SetPruning(pruningOpts)
	}
}

// SetLazyLoading sets if the iavl store should be loaded lazily or not
func (rs *Store) SetLazyLoading(lazyLoading bool) {
	rs.lazyLoading = lazyLoading
}

// Implements Store.
func (rs *Store) GetStoreType() types.StoreType {
	return types.StoreTypeMulti
}

// Implements CommitMultiStore.
func (rs *Store) MountStoreWithDB(key types.StoreKey, typ types.StoreType, db dbm.DB) {
	if key == nil {
		panic("MountIAVLStore() key cannot be nil")
	}
	if _, ok := rs.storesParams[key]; ok {
		panic(fmt.Sprintf("Store duplicate store key %v", key))
	}
	if _, ok := rs.keysByName[key.Name()]; ok {
		panic(fmt.Sprintf("Store duplicate store key name %v", key))
	}
	rs.storesParams[key] = storeParams{
		key: key,
		typ: typ,
		db:  db,
	}
	rs.keysByName[key.Name()] = key
}

// GetCommitStore returns a mounted CommitStore for a given StoreKey. If the
// store is wrapped in an inter-block cache, it will be unwrapped before returning.
func (rs *Store) GetCommitStore(key types.StoreKey) types.CommitStore {
	return rs.GetCommitKVStore(key)
}

// GetCommitKVStore returns a mounted CommitKVStore for a given StoreKey. If the
// store is wrapped in an inter-block cache, it will be unwrapped before returning.
func (rs *Store) GetCommitKVStore(key types.StoreKey) types.CommitKVStore {
	// If the Store has an inter-block cache, first attempt to lookup and unwrap
	// the underlying CommitKVStore by StoreKey. If it does not exist, fallback to
	// the main mapping of CommitKVStores.
	if rs.interBlockCache != nil {
		if store := rs.interBlockCache.Unwrap(key); store != nil {
			return store
		}
	}

	return rs.stores[key]
}

// LoadLatestVersionAndUpgrade implements CommitMultiStore
func (rs *Store) LoadLatestVersionAndUpgrade(upgrades *types.StoreUpgrades) error {
	ver := getLatestVersion(rs.db)
	return rs.loadVersion(ver, upgrades)
}

// LoadVersionAndUpgrade allows us to rename substores while loading an older version
func (rs *Store) LoadVersionAndUpgrade(ver int64, upgrades *types.StoreUpgrades) error {
	return rs.loadVersion(ver, upgrades)
}

// LoadLatestVersion implements CommitMultiStore.
func (rs *Store) LoadLatestVersion() error {
	ver := getLatestVersion(rs.db)
	return rs.loadVersion(ver, nil)
}

// LoadVersion implements CommitMultiStore.
func (rs *Store) LoadVersion(ver int64) error {
	return rs.loadVersion(ver, nil)
}

func (rs *Store) loadVersion(ver int64, upgrades *types.StoreUpgrades) error {
	infos := make(map[string]storeInfo)
	var lastCommitID types.CommitID

	// load old data if we are not version 0
	if ver != 0 {
		cInfo, err := getCommitInfo(rs.db, ver)
		if err != nil {
			return err
		}

		// convert StoreInfos slice to map
		for _, storeInfo := range cInfo.StoreInfos {
			infos[storeInfo.Name] = storeInfo
		}
		lastCommitID = cInfo.CommitID()
	}

	// load each Store (note this doesn't panic on unmounted keys now)
	var newStores = make(map[types.StoreKey]types.CommitKVStore)
	for key, storeParams := range rs.storesParams {
		// Load it
		store, err := rs.loadCommitStoreFromParams(key, rs.getCommitID(infos, key.Name()), storeParams)
		if err != nil {
			return fmt.Errorf("failed to load Store: %v", err)
		}
		newStores[key] = store

		// If it was deleted, remove all data
		if upgrades.IsDeleted(key.Name()) {
			if err := deleteKVStore(store.(types.KVStore)); err != nil {
				return fmt.Errorf("failed to delete store %s: %v", key.Name(), err)
			}
		} else if oldName := upgrades.RenamedFrom(key.Name()); oldName != "" {
			// handle renames specially
			// make an unregistered key to satify loadCommitStore params
			oldKey := types.NewKVStoreKey(oldName)
			oldParams := storeParams
			oldParams.key = oldKey

			// load from the old name
			oldStore, err := rs.loadCommitStoreFromParams(oldKey, rs.getCommitID(infos, oldName), oldParams)
			if err != nil {
				return fmt.Errorf("failed to load old Store '%s': %v", oldName, err)
			}

			// move all data
			if err := moveKVStoreData(oldStore.(types.KVStore), store.(types.KVStore)); err != nil {
				return fmt.Errorf("failed to move store %s -> %s: %v", oldName, key.Name(), err)
			}
		}
	}

	rs.lastCommitID = lastCommitID
	rs.stores = newStores

	return nil
}

func (rs *Store) getCommitID(infos map[string]storeInfo, name string) types.CommitID {
	info, ok := infos[name]
	if !ok {
		return types.CommitID{}
	}
	return info.Core.CommitID
}

func deleteKVStore(kv types.KVStore) error {
	// Note that we cannot write while iterating, so load all keys here, delete below
	var keys [][]byte
	itr := kv.Iterator(nil, nil)
	for itr.Valid() {
		keys = append(keys, itr.Key())
		itr.Next()
	}
	itr.Close()

	for _, k := range keys {
		kv.Delete(k)
	}
	return nil
}

// we simulate move by a copy and delete
func moveKVStoreData(oldDB types.KVStore, newDB types.KVStore) error {
	// we read from one and write to another
	itr := oldDB.Iterator(nil, nil)
	for itr.Valid() {
		newDB.Set(itr.Key(), itr.Value())
		itr.Next()
	}
	itr.Close()

	// then delete the old store
	return deleteKVStore(oldDB)
}

// SetInterBlockCache sets the Store's internal inter-block (persistent) cache.
// When this is defined, all CommitKVStores will be wrapped with their respective
// inter-block cache.
func (rs *Store) SetInterBlockCache(c types.MultiStorePersistentCache) {
	rs.interBlockCache = c
}

// SetTracer sets the tracer for the MultiStore that the underlying
// stores will utilize to trace operations. A MultiStore is returned.
func (rs *Store) SetTracer(w io.Writer) types.MultiStore {
	rs.traceWriter = w
	return rs
}

// SetTracingContext updates the tracing context for the MultiStore by merging
// the given context with the existing context by key. Any existing keys will
// be overwritten. It is implied that the caller should update the context when
// necessary between tracing operations. It returns a modified MultiStore.
func (rs *Store) SetTracingContext(tc types.TraceContext) types.MultiStore {
	if rs.traceContext != nil {
		for k, v := range tc {
			rs.traceContext[k] = v
		}
	} else {
		rs.traceContext = tc
	}

	return rs
}

// TracingEnabled returns if tracing is enabled for the MultiStore.
func (rs *Store) TracingEnabled() bool {
	return rs.traceWriter != nil
}

//----------------------------------------
// +CommitStore

// Implements Committer/CommitStore.
func (rs *Store) LastCommitID() types.CommitID {
	return rs.lastCommitID
}

// Implements Committer/CommitStore.
func (rs *Store) Commit() types.CommitID {

	// Commit stores.
	version := rs.lastCommitID.Version + 1
	commitInfo := commitStores(version, rs.stores)

	// Need to update atomically.
	batch := rs.db.NewBatch()
	defer batch.Close()
	setCommitInfo(batch, version, commitInfo)
	setLatestVersion(batch, version)
	batch.Write()

	// Prepare for next version.
	commitID := types.CommitID{
		Version: version,
		Hash:    commitInfo.Hash(),
	}
	rs.lastCommitID = commitID
	return commitID
}

// Implements CacheWrapper/Store/CommitStore.
func (rs *Store) CacheWrap() types.CacheWrap {
	return rs.CacheMultiStore().(types.CacheWrap)
}

// CacheWrapWithTrace implements the CacheWrapper interface.
func (rs *Store) CacheWrapWithTrace(_ io.Writer, _ types.TraceContext) types.CacheWrap {
	return rs.CacheWrap()
}

//----------------------------------------
// +Snapshotter

// Implements Snapshotter.
func (rs *Store) Snapshot(commitID types.CommitID, dir string) error {
	if dir == "" {
		return errors.New("Path to snapshot directory not given")
	}

	// Format 1
	dir = path.Join(dir, fmt.Sprintf("%d/1", commitID.Version))
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return errors.Wrapf(err, "Failed to create snapshot directory %q", dir)
	}

	chunk := 0
	for key := range rs.stores {
		store, ok := rs.GetStore(key).(*iavl.Store)
		if !ok {
			fmt.Printf("Skipping snapshot of %q\n", key.Name())
			continue
		}

		chunkDir := path.Join(dir, fmt.Sprintf("%d", chunk))
		err = os.MkdirAll(chunkDir, 0755)
		if err != nil {
			return errors.Wrapf(err, "Failed to create snapshot chunk directory %q", chunkDir)
		}
		export, err := store.Export(commitID.Version)
		if err != nil {
			return err
		}
		chunkFile, err := os.Create(path.Join(chunkDir, "data"))
		if err != nil {
			return errors.Wrap(err, "Failed to create chunk file")
		}
		defer chunkFile.Close()
		hasher := sha1.New() // nolint: gosec
		encoder := gob.NewEncoder(io.MultiWriter(chunkFile, hasher))
		err = encoder.Encode(SnapshotChunk{
			Store:   key.Name(),
			Version: commitID.Version,
			Items:   export,
		})
		if err != nil {
			return errors.Wrapf(err, "Failed to encode chunk data for chunk %v", chunk)
		}

		err = ioutil.WriteFile(
			path.Join(chunkDir, "checksum"), []byte(fmt.Sprintf("%x", hasher.Sum(nil))), 0644)
		if err != nil {
			return errors.Wrapf(err, "Failed to write chunk %v checksum", chunk)
		}

		snapshotFile, err := os.Create(path.Join(dir, "metadata"))
		if err != nil {
			return errors.Wrap(err, "Failed to create snapshot metadata file")
		}
		encoder = gob.NewEncoder(snapshotFile)
		err = encoder.Encode(Snapshot{})
		if err != nil {
			return errors.Wrap(err, "Failed to encode snapshot metadata")
		}

		chunk++
	}
	return nil
}

// Implements Snapshotter
func (rs *Store) Restore(data []byte) ([]byte, error) {
	buf := bytes.NewBuffer(data)
	chunk := SnapshotChunk{}
	err := gob.NewDecoder(buf).Decode(&chunk)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode chunk")
	}
	// FIXME Check if store exists
	err = rs.GetStore(types.NewKVStoreKey(chunk.Store)).(*iavl.Store).Import(chunk.Version, chunk.Items)
	if err != nil {
		return nil, err
	}
	// Calculate the root hash (FIXME copied from CommitInfo.Hash and storeInfo.Hash)
	m := make(map[string][]byte, len(rs.stores))
	for key, store := range rs.stores {
		if store.GetStoreType() == types.StoreTypeTransient {
			continue
		}
		// FIXME Apparently we have to hash the commit ID hash 🤷‍♀️
		hasher := tmhash.New()
		_, err := hasher.Write(store.LastCommitID().Hash)
		if err != nil {
			return nil, err
		}
		m[key.Name()] = hasher.Sum(nil)
	}

	return merkle.SimpleHashFromMap(m), nil
}

type Snapshot struct{}

type SnapshotChunk struct {
	Store   string
	Version int64
	Items   []tmiavl.ExportItem
}

//----------------------------------------
// +MultiStore

// CacheMultiStore cache-wraps the multi-store and returns a CacheMultiStore.
// It implements the MultiStore interface.
func (rs *Store) CacheMultiStore() types.CacheMultiStore {
	stores := make(map[types.StoreKey]types.CacheWrapper)
	for k, v := range rs.stores {
		stores[k] = v
	}

	return cachemulti.NewStore(rs.db, stores, rs.keysByName, rs.traceWriter, rs.traceContext)
}

// CacheMultiStoreWithVersion is analogous to CacheMultiStore except that it
// attempts to load stores at a given version (height). An error is returned if
// any store cannot be loaded. This should only be used for querying and
// iterating at past heights.
func (rs *Store) CacheMultiStoreWithVersion(version int64) (types.CacheMultiStore, error) {
	cachedStores := make(map[types.StoreKey]types.CacheWrapper)
	for key, store := range rs.stores {
		switch store.GetStoreType() {
		case types.StoreTypeIAVL:
			// If the store is wrapped with an inter-block cache, we must first unwrap
			// it to get the underlying IAVL store.
			store = rs.GetCommitKVStore(key)

			// Attempt to lazy-load an already saved IAVL store version. If the
			// version does not exist or is pruned, an error should be returned.
			iavlStore, err := store.(*iavl.Store).GetImmutable(version)
			if err != nil {
				return nil, err
			}

			cachedStores[key] = iavlStore

		default:
			cachedStores[key] = store
		}
	}

	return cachemulti.NewStore(rs.db, cachedStores, rs.keysByName, rs.traceWriter, rs.traceContext), nil
}

// GetStore returns a mounted Store for a given StoreKey. If the StoreKey does
// not exist, it will panic. If the Store is wrapped in an inter-block cache, it
// will be unwrapped prior to being returned.
//
// TODO: This isn't used directly upstream. Consider returning the Store as-is
// instead of unwrapping.
func (rs *Store) GetStore(key types.StoreKey) types.Store {
	store := rs.GetCommitKVStore(key)
	if store == nil {
		panic(fmt.Sprintf("store does not exist for key: %s", key.Name()))
	}

	return store
}

// GetKVStore returns a mounted KVStore for a given StoreKey. If tracing is
// enabled on the KVStore, a wrapped TraceKVStore will be returned with the root
// store's tracer, otherwise, the original KVStore will be returned.
//
// NOTE: The returned KVStore may be wrapped in an inter-block cache if it is
// set on the root store.
func (rs *Store) GetKVStore(key types.StoreKey) types.KVStore {
	store := rs.stores[key].(types.KVStore)

	if rs.TracingEnabled() {
		store = tracekv.NewStore(store, rs.traceWriter, rs.traceContext)
	}

	return store
}

// getStoreByName performs a lookup of a StoreKey given a store name typically
// provided in a path. The StoreKey is then used to perform a lookup and return
// a Store. If the Store is wrapped in an inter-block cache, it will be unwrapped
// prior to being returned. If the StoreKey does not exist, nil is returned.
func (rs *Store) getStoreByName(name string) types.Store {
	key := rs.keysByName[name]
	if key == nil {
		return nil
	}

	return rs.GetCommitKVStore(key)
}

//---------------------- Query ------------------

// Query calls substore.Query with the same `req` where `req.Path` is
// modified to remove the substore prefix.
// Ie. `req.Path` here is `/<substore>/<path>`, and trimmed to `/<path>` for the substore.
// TODO: add proof for `multistore -> substore`.
func (rs *Store) Query(req abci.RequestQuery) abci.ResponseQuery {
	// Query just routes this to a substore.
	path := req.Path
	storeName, subpath, err := parsePath(path)
	if err != nil {
		return sdkerrors.QueryResult(err)
	}

	store := rs.getStoreByName(storeName)
	if store == nil {
		return sdkerrors.QueryResult(sdkerrors.Wrapf(sdkerrors.ErrUnknownRequest, "no such store: %s", storeName))
	}

	queryable, ok := store.(types.Queryable)
	if !ok {
		return sdkerrors.QueryResult(sdkerrors.Wrapf(sdkerrors.ErrUnknownRequest, "store %s (type %T) doesn't support queries", storeName, store))
	}

	// trim the path and make the query
	req.Path = subpath
	res := queryable.Query(req)

	if !req.Prove || !RequireProof(subpath) {
		return res
	}

	if res.Proof == nil || len(res.Proof.Ops) == 0 {
		return sdkerrors.QueryResult(sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "proof is unexpectedly empty; ensure height has not been pruned"))
	}

	commitInfo, errMsg := getCommitInfo(rs.db, res.Height)
	if errMsg != nil {
		return sdkerrors.QueryResult(err)
	}

	// Restore origin path and append proof op.
	res.Proof.Ops = append(res.Proof.Ops, NewMultiStoreProofOp(
		[]byte(storeName),
		NewMultiStoreProof(commitInfo.StoreInfos),
	).ProofOp())

	// TODO: handle in another TM v0.26 update PR
	// res.Proof = buildMultiStoreProof(res.Proof, storeName, commitInfo.StoreInfos)
	return res
}

// parsePath expects a format like /<storeName>[/<subpath>]
// Must start with /, subpath may be empty
// Returns error if it doesn't start with /
func parsePath(path string) (storeName string, subpath string, err error) {
	if !strings.HasPrefix(path, "/") {
		return storeName, subpath, sdkerrors.Wrapf(sdkerrors.ErrUnknownRequest, "invalid path: %s", path)
	}

	paths := strings.SplitN(path[1:], "/", 2)
	storeName = paths[0]

	if len(paths) == 2 {
		subpath = "/" + paths[1]
	}

	return storeName, subpath, nil
}

//----------------------------------------
// Note: why do we use key and params.key in different places. Seems like there should be only one key used.
func (rs *Store) loadCommitStoreFromParams(key types.StoreKey, id types.CommitID, params storeParams) (types.CommitKVStore, error) {
	var db dbm.DB

	if params.db != nil {
		db = dbm.NewPrefixDB(params.db, []byte("s/_/"))
	} else {
		prefix := "s/k:" + params.key.Name() + "/"
		db = dbm.NewPrefixDB(rs.db, []byte(prefix))
	}

	switch params.typ {
	case types.StoreTypeMulti:
		panic("recursive MultiStores not yet supported")

	case types.StoreTypeIAVL:
		store, err := iavl.LoadStore(db, id, rs.pruningOpts, rs.lazyLoading)
		if err != nil {
			return nil, err
		}

		if rs.interBlockCache != nil {
			// Wrap and get a CommitKVStore with inter-block caching. Note, this should
			// only wrap the primary CommitKVStore, not any store that is already
			// cache-wrapped as that will create unexpected behavior.
			store = rs.interBlockCache.GetStoreCache(key, store)
		}

		return store, err

	case types.StoreTypeDB:
		return commitDBStoreAdapter{Store: dbadapter.Store{DB: db}}, nil

	case types.StoreTypeTransient:
		_, ok := key.(*types.TransientStoreKey)
		if !ok {
			return nil, fmt.Errorf("invalid StoreKey for StoreTypeTransient: %s", key.String())
		}

		return transient.NewStore(), nil

	default:
		panic(fmt.Sprintf("unrecognized store type %v", params.typ))
	}
}

//----------------------------------------
// storeParams

type storeParams struct {
	key types.StoreKey
	db  dbm.DB
	typ types.StoreType
}

//----------------------------------------
// commitInfo

// NOTE: Keep commitInfo a simple immutable struct.
type commitInfo struct {

	// Version
	Version int64

	// Store info for
	StoreInfos []storeInfo
}

// Hash returns the simple merkle root hash of the stores sorted by name.
func (ci commitInfo) Hash() []byte {
	// TODO: cache to ci.hash []byte
	m := make(map[string][]byte, len(ci.StoreInfos))
	for _, storeInfo := range ci.StoreInfos {
		m[storeInfo.Name] = storeInfo.Hash()
	}

	return merkle.SimpleHashFromMap(m)
}

func (ci commitInfo) CommitID() types.CommitID {
	return types.CommitID{
		Version: ci.Version,
		Hash:    ci.Hash(),
	}
}

//----------------------------------------
// storeInfo

// storeInfo contains the name and core reference for an
// underlying store.  It is the leaf of the Stores top
// level simple merkle tree.
type storeInfo struct {
	Name string
	Core storeCore
}

type storeCore struct {
	// StoreType StoreType
	CommitID types.CommitID
	// ... maybe add more state
}

// Implements merkle.Hasher.
func (si storeInfo) Hash() []byte {
	// Doesn't write Name, since merkle.SimpleHashFromMap() will
	// include them via the keys.
	bz := si.Core.CommitID.Hash
	hasher := tmhash.New()

	_, err := hasher.Write(bz)
	if err != nil {
		// TODO: Handle with #870
		panic(err)
	}

	return hasher.Sum(nil)
}

//----------------------------------------
// Misc.

func getLatestVersion(db dbm.DB) int64 {
	var latest int64
	latestBytes, err := db.Get([]byte(latestVersionKey))
	if err != nil {
		panic(err)
	} else if latestBytes == nil {
		return 0
	}

	err = cdc.UnmarshalBinaryLengthPrefixed(latestBytes, &latest)
	if err != nil {
		panic(err)
	}

	return latest
}

// Set the latest version.
func setLatestVersion(batch dbm.Batch, version int64) {
	latestBytes, _ := cdc.MarshalBinaryLengthPrefixed(version)
	batch.Set([]byte(latestVersionKey), latestBytes)
}

// Commits each store and returns a new commitInfo.
func commitStores(version int64, storeMap map[types.StoreKey]types.CommitKVStore) commitInfo {
	storeInfos := make([]storeInfo, 0, len(storeMap))

	for key, store := range storeMap {
		// Commit
		commitID := store.Commit()

		if store.GetStoreType() == types.StoreTypeTransient {
			continue
		}

		// Record CommitID
		si := storeInfo{}
		si.Name = key.Name()
		si.Core.CommitID = commitID
		// si.Core.StoreType = store.GetStoreType()
		storeInfos = append(storeInfos, si)
	}

	ci := commitInfo{
		Version:    version,
		StoreInfos: storeInfos,
	}
	return ci
}

// Gets commitInfo from disk.
func getCommitInfo(db dbm.DB, ver int64) (commitInfo, error) {

	// Get from DB.
	cInfoKey := fmt.Sprintf(commitInfoKeyFmt, ver)
	cInfoBytes, err := db.Get([]byte(cInfoKey))
	if err != nil {
		return commitInfo{}, fmt.Errorf("failed to get commit info: %v", err)
	} else if cInfoBytes == nil {
		return commitInfo{}, fmt.Errorf("failed to get commit info: no data")
	}

	var cInfo commitInfo

	err = cdc.UnmarshalBinaryLengthPrefixed(cInfoBytes, &cInfo)
	if err != nil {
		return commitInfo{}, fmt.Errorf("failed to get Store: %v", err)
	}

	return cInfo, nil
}

// Set a commitInfo for given version.
func setCommitInfo(batch dbm.Batch, version int64, cInfo commitInfo) {
	cInfoBytes := cdc.MustMarshalBinaryLengthPrefixed(cInfo)
	cInfoKey := fmt.Sprintf(commitInfoKeyFmt, version)
	batch.Set([]byte(cInfoKey), cInfoBytes)
}

package sync15

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/juruen/rmapi/archive"
	"github.com/juruen/rmapi/filetree"
	"github.com/juruen/rmapi/log"
	"github.com/juruen/rmapi/model"
	"github.com/juruen/rmapi/transport"
	"github.com/juruen/rmapi/util"
)

// An ApiCtx allows you interact with the remote reMarkable API
type ApiCtx struct {
	Http        *transport.HttpClientCtx
	ft          *filetree.FileTreeCtx
	blobStorage *BlobStorage
	hashTree    *HashTree
}

// max number of concurrent requests
var concurrent = 20

func init() {
	c := os.Getenv("RMAPI_CONCURRENT")
	if u, err := strconv.Atoi(c); err == nil {
		concurrent = u
	}
}

func CreateCtx(http *transport.HttpClientCtx) (*ApiCtx, error) {
	apiStorage := NewBlobStorage(http)
	cacheTree, err := loadTree()
	if err != nil {
		fmt.Print(err)
		return nil, err
	}
	err = cacheTree.Mirror(apiStorage, concurrent)
	if err != nil {
		return nil, fmt.Errorf("failed to mirror %v", err)
	}
	saveTree(cacheTree)
	tree := DocumentsFileTree(cacheTree)
	return &ApiCtx{http, tree, apiStorage, cacheTree}, nil
}

func (ctx *ApiCtx) Filetree() *filetree.FileTreeCtx {
	return ctx.ft
}

func (ctx *ApiCtx) Refresh() (string, int64, error) {
	err := ctx.hashTree.Mirror(ctx.blobStorage, concurrent)
	if err != nil {
		return "", 0, err
	}
	ctx.ft = DocumentsFileTree(ctx.hashTree)
	return ctx.hashTree.Hash, ctx.hashTree.Generation, nil
}

// Nuke removes all documents from the account
func (ctx *ApiCtx) Nuke() (err error) {
	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		ctx.hashTree.Docs = nil
		ctx.hashTree.Rehash()
		return nil
	}, true)
	return err
}

// FetchDocument downloads a document given its ID and saves it locally into dstPath
func (ctx *ApiCtx) FetchDocument(docId, dstPath string) error {
	doc, err := ctx.hashTree.FindDoc(docId)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "rmapizip")

	if err != nil {
		log.Error.Println("failed to create tmpfile for zip dir", err)
		return err
	}
	defer tmp.Close()

	w := zip.NewWriter(tmp)
	defer w.Close()
	for _, f := range doc.Files {
		log.Trace.Println("fetching document: ", f.DocumentID)
		blobReader, err := ctx.blobStorage.GetReader(f.Hash, f.DocumentID)
		if err != nil {
			return err
		}
		defer blobReader.Close()
		header := zip.FileHeader{}
		header.Name = f.DocumentID
		header.Modified = time.Now()
		zipWriter, err := w.CreateHeader(&header)
		if err != nil {
			return err
		}
		_, err = io.Copy(zipWriter, blobReader)

		if err != nil {
			return err
		}
	}
	w.Close()
	tmpPath := tmp.Name()
	_, err = util.CopyFile(tmpPath, dstPath)

	if err != nil {
		log.Error.Printf("failed to copy %s to %s, er: %s\n", tmpPath, dstPath, err.Error())
		return err
	}

	defer os.RemoveAll(tmp.Name())

	return nil
}

// CreateDir creates a remote directory with a given name under the parentId directory
func (ctx *ApiCtx) CreateDir(parentId, name string, notify bool) (*model.Document, error) {
	var err error

	files := &archive.DocumentFiles{}

	tmpDir, err := os.MkdirTemp("", "rmupload")
	if err != nil {
		return nil, err
	}
	id := uuid.New().String()
	objectName, filePath, err := archive.CreateMetadata(id, name, parentId, model.DirectoryType, tmpDir, nil)
	if err != nil {
		return nil, err
	}
	files.AddMap(objectName, filePath, archive.MetadataExt)

	objectName, filePath, err = archive.CreateContent(id, "", tmpDir, nil, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	files.AddMap(objectName, filePath, archive.ContentExt)

	doc := NewBlobDoc(name, id, model.DirectoryType, parentId)

	for _, f := range files.Files {
		log.Info.Printf("File %s, path: %s", f.Name, f.Path)
		hash, size, err := FileHashAndSize(f.Path)
		if err != nil {
			return nil, err
		}
		hashStr := hex.EncodeToString(hash)
		fileEntry := &Entry{
			DocumentID: f.Name,
			Hash:       hashStr,
			Type:       FileType,
			Size:       size,
		}
		reader, err := os.Open(f.Path)
		if err != nil {
			return nil, err
		}
		//does not accept rm-file in header
		err = ctx.blobStorage.UploadBlob(hashStr, f.Name, reader)
		reader.Close()

		if err != nil {
			return nil, err
		}

		doc.AddFile(fileEntry)
	}

	log.Info.Println("Uploading new doc index...", doc.Hash)
	indexReader, err := doc.IndexReader()
	if err != nil {
		return nil, err
	}
	// defer indexReader.Close()
	err = ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	if err != nil {
		return nil, err
	}

	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		return t.Add(doc)
	}, notify)

	if err != nil {
		return nil, err
	}

	if notify {
		err = ctx.SyncComplete()
		if err != nil {
			return nil, err
		}
	}

	return doc.ToDocument(), nil
}

// Sync applies changes to the local tree and syncs with the remote storage
func Sync(b *BlobStorage, tree *HashTree, operation func(t *HashTree) error, notify bool) error {
	syncTry := 0
	for {
		syncTry++
		if syncTry > 10 {
			log.Error.Println("Something is wrong")
			break
		}
		log.Info.Println("Syncing...")
		err := operation(tree)
		if err != nil {
			return err
		}

		indexReader, err := tree.IndexReader()
		if err != nil {
			return err
		}
		err = b.UploadBlob(tree.Hash, addExt("root", archive.DocSchemaExt), indexReader)
		if err != nil {
			return err
		}
		// TODO
		// defer indexReader.Close()

		log.Info.Println("updating root, old gen: ", tree.Generation)

		newGeneration, err := b.WriteRootIndex(tree.Hash, tree.Generation, notify)

		if err == nil {
			log.Info.Println("wrote root, new gen: ", newGeneration)
			tree.Generation = newGeneration
			break
		}

		if err != transport.ErrWrongGeneration {
			return err
		}

		log.Info.Println("wrong generation, re-reading remote tree")
		//resync and try again
		err = tree.Mirror(b, concurrent)
		if err != nil {
			return err
		}
		log.Warning.Println("remote tree has changed, refresh the file tree")
	}
	return saveTree(tree)
}

// DeleteEntry removes an entry: either an empty directory or a file
func (ctx *ApiCtx) DeleteEntry(node *model.Node, recursive, notify bool) error {
	if node.IsDirectory() && len(node.Children) > 0 && !recursive {
		return errors.New("directory is not empty")
	}

	err := Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		return t.Remove(node.Document.ID)
	}, notify)
	return err
}

// MoveEntry moves an entry (either a directory or a file)
// - src is the source node to be moved
// - dstDir is an existing destination directory
// - name is the new name of the moved entry in the destination directory
func (ctx *ApiCtx) MoveEntry(src, dstDir *model.Node, name string) (*model.Node, error) {
	if dstDir.IsFile() {
		return nil, errors.New("destination directory is a file")
	}
	var err error

	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		doc, err := t.FindDoc(src.Document.ID)
		if err != nil {
			return err
		}
		doc.Metadata.Version++
		doc.Metadata.DocName = name
		doc.Metadata.Parent = dstDir.Id()
		doc.Metadata.MetadataModified = true

		hashStr, reader, err := doc.MetadataHashAndReader()
		if err != nil {
			return err
		}
		err = doc.Rehash()
		if err != nil {
			return err
		}
		err = t.Rehash()

		if err != nil {
			return err
		}

		err = ctx.blobStorage.UploadBlob(hashStr, addExt(doc.DocumentID, archive.MetadataExt), reader)

		if err != nil {
			return err
		}

		log.Info.Println("Uploading new doc index...", doc.Hash)
		indexReader, err := doc.IndexReader()
		if err != nil {
			return err
		}
		// defer indexReader.Close()
		return ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	}, true)

	if err != nil {
		return nil, err
	}

	d, err := ctx.hashTree.FindDoc(src.Document.ID)
	if err != nil {
		return nil, err
	}

	return &model.Node{Document: d.ToDocument(), Children: src.Children, Parent: dstDir}, nil
}

// UploadDocument uploads a local document given by sourceDocPath under the parentId directory
func (ctx *ApiCtx) UploadDocument(parentId string, sourceDocPath string, notify bool, coverpage *int, currentPage *int, pageCount *int, contrastFilter *string) (*model.Document, error) {
	//TODO: overwrite file
	name, ext := util.DocPathToName(sourceDocPath)

	if name == "" {
		return nil, errors.New("file name is invalid")
	}

	if !util.IsFileTypeSupported(ext) {
		return nil, errors.New("unsupported file extension: " + ext)
	}

	var err error

	tmpDir, err := os.MkdirTemp("", "rmupload")
	if err != nil {
		return nil, err
	}

	defer os.RemoveAll(tmpDir)

	docFiles, id, err := archive.Prepare(name, parentId, sourceDocPath, ext, tmpDir, coverpage, currentPage, pageCount, contrastFilter)
	if err != nil {
		return nil, err
	}

	doc := NewBlobDoc(name, id, model.DocumentType, parentId)
	for _, f := range docFiles.Files {
		log.Info.Printf("File %s, path: %s", f.Name, f.Path)
		hash, size, err := FileHashAndSize(f.Path)
		if err != nil {
			return nil, err
		}
		hashStr := hex.EncodeToString(hash)
		fileEntry := &Entry{
			DocumentID: f.Name,
			Hash:       hashStr,
			Type:       FileType,
			Size:       size,
		}
		reader, err := os.Open(f.Path)
		if err != nil {
			return nil, err
		}
		err = ctx.blobStorage.UploadBlob(hashStr, fileEntry.DocumentID, reader)

		if err != nil {
			return nil, err
		}

		doc.AddFile(fileEntry)
	}

	log.Info.Printf("Uploading new doc index...%s, size: %d", doc.Hash, doc.Size)
	indexReader, err := doc.IndexReader()
	if err != nil {
		return nil, err
	}
	// defer indexReader.Close()
	err = ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	if err != nil {
		return nil, err
	}

	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		return t.Add(doc)
	}, notify)

	if err != nil {
		return nil, err
	}

	return doc.ToDocument(), nil
}

// ReplaceDocumentFile replaces the main document file (e.g. PDF) of an existing document
// identified by docId with the local file given by sourceDocPath. Metadata and annotations
// remain untouched.
func (ctx *ApiCtx) ReplaceDocumentFile(docId, sourceDocPath string, notify bool) error {
	_, ext := util.DocPathToName(sourceDocPath)
	return Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		doc, err := t.FindDoc(docId)
		if err != nil {
			return err
		}

		var fileEntry *Entry
		for _, f := range doc.Files {
			if strings.HasSuffix(f.DocumentID, "."+ext) {
				fileEntry = f
				break
			}
		}
		if fileEntry == nil {
			return fmt.Errorf("document does not contain .%s", ext)
		}

		hash, size, err := FileHashAndSize(sourceDocPath)
		if err != nil {
			return err
		}
		hashStr := hex.EncodeToString(hash)

		r, err := os.Open(sourceDocPath)
		if err != nil {
			return err
		}
		defer r.Close()

		if err := ctx.blobStorage.UploadBlob(hashStr, fileEntry.DocumentID, r); err != nil {
			return err
		}

		fileEntry.Hash = hashStr
		fileEntry.Size = size

		if err := doc.Rehash(); err != nil {
			return err
		}
		if err := t.Rehash(); err != nil {
			return err
		}

		indexReader, err := doc.IndexReader()
		if err != nil {
			return err
		}
		return ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	}, notify)
}

// errTagWriteNoop aborts a Sync closure whose tag operation turned out to
// change nothing once the tree was current. Sync always rewrites the root
// index after a successful closure, which would bump the generation for a
// write that wrote nothing; returning an error leaves the remote untouched.
var errTagWriteNoop = errors.New("tags already as requested")

// readbackRetryDelays governs how long verifyTagWriteLanded waits between
// retries when a post-commit readback contradicts a write this process just
// established, as a local fact, actually landed: the root has moved, but to
// a generation at or before the one this write committed against. That is a
// replica-lag symptom on the read path, not evidence the write never
// landed. Tests set this to zeros.
var readbackRetryDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// SupersededError reports that this write landed, and a later writer has
// since replaced docId's revision. Unlike round 3, this is no longer
// ambiguous: verifyTagWriteLanded only reaches this branch after
// UpdateDocumentTags has already established, as a local fact (not a
// server round-trip), that this write's own root commit succeeded — see
// UpdateDocumentTags. A root that has since moved past that generation,
// with docId's entry no longer matching what this write produced, can only
// mean a later writer replaced it.
type SupersededError struct {
	DocumentID          string
	ExpectedRevision    string // the revision this write produced
	ActualRevision      string // what the remote root now lists
	CommittedGeneration int64
	CurrentGeneration   int64
}

func (e *SupersededError) Error() string {
	return fmt.Sprintf(
		"superseded: this write landed at generation %d, and a later writer has since replaced %s's revision (now %s; this write produced %s), advancing the root to generation %d",
		e.CommittedGeneration, e.DocumentID, e.ActualRevision, e.ExpectedRevision, e.CurrentGeneration)
}

// NotCommittedError reports that a tag write never reached the server at
// all. UpdateDocumentTags decides this from a local fact, not a network
// round-trip: after Sync returns without error, ctx.hashTree.Hash is either
// the root this write just committed (attemptedRoot) on genuine success, or
// — on Sync's syncTry>10 fail-open — the server's real root, left there by
// the last of Sync's own retries mirroring the tree before giving up. A
// mismatch between the two means this write's root PUT never landed. Unlike
// SupersededError, nobody else could have advanced the generation either,
// so this case is unambiguous.
type NotCommittedError struct {
	DocumentID       string
	ExpectedRevision string
	ActualRevision   string
}

func (e *NotCommittedError) Error() string {
	return fmt.Sprintf(
		"not committed: remote root lists revision %s for %s, this write produced %s",
		e.ActualRevision, e.DocumentID, e.ExpectedRevision)
}

// UpdateDocumentTags applies a tag operation to one document with a
// stale-revision precondition, and returns a structured before/after report.
//
// Nothing in the tree is mutated until all three blobs (content, metadata,
// doc index) have uploaded: plan.apply runs last inside the closure, so a
// failed upload leaves the tree exactly as it found it. If Sync itself later
// fails with a genuine (non-412) error, the applied plan is rolled back —
// unless a readback shows the write actually landed and only its response
// was lost; see readbackAfterSyncError.
//
// Sync's return value alone cannot answer "did this land": after ten
// generation conflicts it fails open and returns nil despite never writing
// the root. What it does leave behind is trustworthy locally, with no
// server round-trip: on genuine success ctx.hashTree.Hash is the root this
// write just committed (attemptedRoot); on the fail-open, Sync's last retry
// already mirrored the tree to the server's true root before giving up, so
// ctx.hashTree.Hash != attemptedRoot is a local fact, checked first. Only
// once that fact says the write committed does readback go to the server,
// to confirm what landed and classify what may have happened since — see
// verifyTagWriteLanded.
func (ctx *ApiCtx) UpdateDocumentTags(docId string, op TagOp, tags []string, expectedRevision string) (*TagUpdateResult, error) {
	if os.Getenv("RMAPI_FORCE_SCHEMA_VERSION") != "" {
		return nil, errors.New("refusing to write tags with RMAPI_FORCE_SCHEMA_VERSION set; it exists to inspect failure modes and would change the root index body this write publishes")
	}

	var result *TagUpdateResult
	var plan *tagUpdatePlan
	var d *BlobDoc
	var attemptedRoot string
	applied := false
	// Computed once, outside Sync's retry loop: a retry then re-PUTs the
	// same content-addressed bytes this attempt already produced, instead
	// of minting a fresh orphan blob (and a fresh lastModified) on every
	// generation conflict.
	now := time.Now().UnixMilli()

	err := Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		applied = false

		// A stale local root is refreshed here, before any bytes are built
		// or uploaded: a wasted three-blob upload followed by a 412 costs
		// the same round trip a plain refresh would, and it is what makes
		// both the containment check below and a no-op decision
		// trustworthy — they reason about the tree this attempt is about
		// to try to commit, not a cache that has already drifted.
		rootHash, _, err := ctx.blobStorage.GetRootIndex()
		if err != nil {
			return err
		}
		if rootHash != t.Hash {
			// Mirror reads the root itself, so if the server moved between
			// the two reads the tree lands on the newer root; that is fine —
			// containment below checks the tree against the server, and a
			// stale generation still gets the 412 from WriteRootIndex.
			if err := t.Mirror(ctx.blobStorage, concurrent); err != nil {
				return err
			}
		}

		d, err = t.FindDoc(docId)
		if err != nil {
			return err
		}

		if err := ctx.assertRootContainment(t, docId, d); err != nil {
			return err
		}

		remoteFiles, err := ctx.fetchRemoteDocIndex(d)
		if err != nil {
			return err
		}
		remoteHash, err := HashEntries(remoteFiles)
		if err != nil {
			return err
		}
		if remoteHash != d.Hash {
			return fmt.Errorf("remote index for %s hashes to %s, root lists %s", docId, remoteHash, d.Hash)
		}

		rawMetadata, err := ctx.fetchDocFile(docId, remoteFiles, archive.MetadataExt)
		if err != nil {
			return err
		}
		// The kind lives in the raw bytes, not d.Metadata: Mirror may skip
		// re-parsing a document whose hash it already recognises.
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawMetadata, &kind); err != nil {
			return fmt.Errorf("%s: %w", addExt(docId, archive.MetadataExt), err)
		}
		if kind.Type != model.DocumentType {
			return fmt.Errorf("%s is a %q, not a document", docId, kind.Type)
		}

		rawContent, err := ctx.fetchDocFile(docId, remoteFiles, archive.ContentExt)
		if err != nil {
			return err
		}

		plan, err = planTagUpdate(d, remoteFiles, rawContent, rawMetadata, op, tags, expectedRevision, now)
		if err != nil {
			return err
		}
		plan.generation = t.Generation
		result = plan.Result
		if !plan.Result.Changed {
			return errTagWriteNoop
		}

		if err := verifyTagSplice(rawContent, plan.Content, plan.Result.AfterTags); err != nil {
			return err
		}
		if err := verifyMetadataSplice(rawMetadata, plan.Metadata, now); err != nil {
			return err
		}

		if err := ctx.blobStorage.UploadBlob(plan.Result.ContentHash, addExt(d.DocumentID, archive.ContentExt), bytes.NewReader(plan.Content)); err != nil {
			return err
		}
		if err := ctx.blobStorage.UploadBlob(plan.Result.MetadataHash, addExt(d.DocumentID, archive.MetadataExt), bytes.NewReader(plan.Metadata)); err != nil {
			return err
		}

		idx, err := plan.indexReader()
		if err != nil {
			return err
		}
		if err := ctx.blobStorage.UploadBlob(plan.docHash, addExt(d.DocumentID, archive.DocSchemaExt), idx); err != nil {
			return err
		}

		plan.apply(d)
		applied = true
		if err := t.Rehash(); err != nil {
			return err
		}
		// Captured now, not read back from ctx.hashTree.Hash after Sync
		// returns: a generation conflict on this attempt makes Sync mirror
		// the tree before retrying, which overwrites t.Hash with the
		// server's (still-unwritten) state. This is the hash Sync is about
		// to try to commit for this attempt, win or lose.
		attemptedRoot = t.Hash
		return nil
	}, true)

	if errors.Is(err, errTagWriteNoop) {
		log.Info.Println("tags already as requested; nothing to write")
		rootHash, _, rErr := ctx.blobStorage.GetRootIndex()
		if rErr != nil {
			return result, fmt.Errorf("readback: %w", rErr)
		}
		if rootHash != ctx.hashTree.Hash {
			return result, errors.New("local cache does not match the server root; refusing to report a no-op")
		}
		return result, nil
	}
	if err != nil {
		if applied && plan != nil && d != nil {
			landed, rErr := ctx.readbackAfterSyncError(docId, plan, attemptedRoot)
			if landed {
				// The root PUT actually landed; what Sync reported was a
				// lost ack, not a lost write. ctx.hashTree.Generation is
				// stale until the next Mirror, which is fine for a
				// one-shot CLI — everything else already reflects what the
				// server holds, and rolling back now would only disagree
				// with a server this write itself wrote to successfully.
				return plan.Result, nil
			}
			if rErr != nil {
				err = fmt.Errorf("%w (post-error readback: %v)", err, rErr)
			}
			plan.rollback(d)
			ctx.hashTree.Rehash()
		}
		return nil, err
	}

	if ctx.hashTree.Hash != attemptedRoot {
		// A local fact, not a network round-trip: Sync's own syncTry>10
		// fail-open already mirrored the tree to the server's true root
		// before giving up. The tree is left as Sync's own retries found
		// it, on purpose — it already reflects server state, so rolling it
		// back here would clobber that.
		actual := ""
		if ad, aerr := ctx.hashTree.FindDoc(docId); aerr == nil {
			actual = ad.Hash
		}
		return plan.Result, &NotCommittedError{
			DocumentID:       docId,
			ExpectedRevision: plan.Result.AfterRevision,
			ActualRevision:   actual,
		}
	}

	result, err = ctx.verifyTagWriteLanded(docId, plan, attemptedRoot)
	if err != nil {
		return result, err
	}
	return result, nil
}

// readbackAfterSyncError checks, after Sync itself returned a non-412
// error, whether the root PUT this attempt made actually landed anyway: a
// network failure between the server accepting the write and its response
// reaching this process is indistinguishable, from Sync's point of view,
// from a genuinely rejected write. Rolling back a write that landed would
// leave the local cache disagreeing with a server it just wrote to
// successfully, so this is checked before any rollback happens. landed is
// true only once the write set itself — not just the root pointer — has
// been verified by bytes.
func (ctx *ApiCtx) readbackAfterSyncError(docId string, plan *tagUpdatePlan, attemptedRoot string) (landed bool, readbackErr error) {
	rootHash, _, err := ctx.blobStorage.GetRootIndex()
	if err != nil {
		return false, err
	}
	if rootHash != attemptedRoot {
		return false, nil
	}
	if err := ctx.verifyWriteSetLanded(docId, plan); err != nil {
		return false, err
	}
	return true, nil
}

// fetchRemoteDocIndex reads a document's file index from the server at its
// currently cached hash — the base a tag write plans against, so plan sizes
// and membership come from what the server holds, not the local cache.
func (ctx *ApiCtx) fetchRemoteDocIndex(d *BlobDoc) ([]*Entry, error) {
	r, err := ctx.blobStorage.GetReader(d.Hash, addExt(d.DocumentID, archive.DocSchemaExt))
	if err != nil {
		return nil, fmt.Errorf("fetch remote index for %s: %w", d.DocumentID, err)
	}
	defer r.Close()
	entries, _, err := parseIndex(r)
	if err != nil {
		return nil, fmt.Errorf("fetch remote index for %s: %w", d.DocumentID, err)
	}
	return entries, nil
}

// assertRootContainment reads the remote root index at the tree's own hash
// and cross-checks the local cache against it, exhaustively, before any
// upload: it proves that every entry the server lists is present locally at
// the same revision, that the local cache names nothing extra, and that
// docId's own entry is among them — so the root this write is about to
// publish will re-list only what the server already holds, nothing more and
// nothing stale. This catches a truncated, corrupted, or over-full local
// cache: one that still names a valid, existing server root, but no longer
// agrees with everything that root lists.
func (ctx *ApiCtx) assertRootContainment(t *HashTree, docId string, d *BlobDoc) error {
	rootReader, err := ctx.blobStorage.GetReader(t.Hash, addExt("root", archive.DocSchemaExt))
	if err != nil {
		return fmt.Errorf("root containment check: %w", err)
	}
	defer rootReader.Close()
	remoteRoot, _, err := parseIndex(rootReader)
	if err != nil {
		return fmt.Errorf("root containment check: %w", err)
	}

	localByID := make(map[string]*BlobDoc, len(t.Docs))
	for _, ld := range t.Docs {
		localByID[ld.DocumentID] = ld
	}
	remoteByID := make(map[string]*Entry, len(remoteRoot))

	var docEntry *Entry
	for _, e := range remoteRoot {
		remoteByID[e.DocumentID] = e
		local, ok := localByID[e.DocumentID]
		if !ok {
			return fmt.Errorf(
				"local cache is missing a document the server has (%s); delete tree.cache and retry",
				e.DocumentID)
		}
		if local.Hash != e.Hash {
			return fmt.Errorf(
				"cached revision %s for %s does not match the server's %s; delete tree.cache and retry",
				local.Hash, e.DocumentID, e.Hash)
		}
		if e.DocumentID == docId {
			docEntry = e
		}
	}

	for id := range localByID {
		if _, ok := remoteByID[id]; !ok {
			return fmt.Errorf(
				"local cache has a document the server does not list (%s); delete tree.cache and retry",
				id)
		}
	}

	if docEntry == nil {
		return fmt.Errorf("server root does not list %s; delete tree.cache and retry", docId)
	}
	return nil
}

// fetchDocFile reads one of a document's files, from entries (the document's
// file list as read from the server, not a possibly-stale local cache), by
// the hash its entry carries.
func (ctx *ApiCtx) fetchDocFile(docID string, entries []*Entry, ext archive.RmExt) ([]byte, error) {
	name := addExt(docID, ext)
	for _, f := range entries {
		if f.DocumentID != name {
			continue
		}
		r, err := ctx.blobStorage.GetReader(f.Hash, f.DocumentID)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", name, err)
		}
		defer r.Close()
		raw, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		return raw, nil
	}
	return nil, fmt.Errorf("document %s has no %s file", docID, name)
}

// verifyTagWriteLanded is the post-commit readback. It is only ever called
// once UpdateDocumentTags has already established, as a local fact rather
// than a network round-trip, that this write's own root commit succeeded
// (ctx.hashTree.Hash == attemptedRoot) — so every branch below is about
// confirming what the server now holds and classifying what may have
// changed since, never about whether this write itself landed.
//
// The two blobs this write actually produced are verified by their bytes,
// not just the hash chain: a server that acknowledges a blob PUT and
// silently drops it would otherwise leave the root pointing at a doc index
// whose .content or .metadata 404s — a broken notebook reported as success.
// See verifyWriteSetLanded, which every landed branch below calls before
// returning success.
//
// If the live root hash already equals attemptedRoot, the write landed at
// the root level with nothing else in between. Otherwise something else has
// moved the root since; the readback follows it to docId's own entry. If
// that entry is still at the revision this write produced, the write landed
// regardless of who moved the root elsewhere.
//
// If not, and the root's generation has moved past the one this write
// committed at, a later writer really has replaced docId's revision
// (SupersededError — unambiguous now, since this write's own commit was
// already confirmed before this function was ever called). If the
// generation has NOT moved past that point, the root and docId's entry
// disagreeing with what a just-committed write produced is a contradiction,
// not evidence the write never landed — a read replica lagging the write
// path, most likely. That case is retried up to len(readbackRetryDelays)
// times before giving up with a plain error.
func (ctx *ApiCtx) verifyTagWriteLanded(docId string, plan *tagUpdatePlan, attemptedRoot string) (*TagUpdateResult, error) {
	// The generation this write's commit produced — Sync stores the server's
	// reply on success. That, not plan.generation (the base the write was
	// planned against), is the line a "later writer" has to be past: a
	// readback at exactly this generation that still disagrees with the
	// write is a contradiction, not a supersession.
	committed := ctx.hashTree.Generation
	for attempt := 0; ; attempt++ {
		rootHash, generation, err := ctx.blobStorage.GetRootIndex()
		if err != nil {
			return plan.Result, fmt.Errorf("readback: %w", err)
		}
		if rootHash == attemptedRoot {
			if err := ctx.verifyWriteSetLanded(docId, plan); err != nil {
				return plan.Result, err
			}
			return plan.Result, nil
		}

		rootReader, err := ctx.blobStorage.GetReader(rootHash, addExt("root", archive.DocSchemaExt))
		if err != nil {
			return plan.Result, fmt.Errorf("readback: %w", err)
		}
		rootEntries, _, err := parseIndex(rootReader)
		rootReader.Close()
		if err != nil {
			return plan.Result, fmt.Errorf("readback: %w", err)
		}

		var docEntry *Entry
		for _, e := range rootEntries {
			if e.DocumentID == docId {
				docEntry = e
				break
			}
		}
		if docEntry == nil {
			return plan.Result, fmt.Errorf("readback: remote root does not list %s", docId)
		}
		if docEntry.Hash == plan.Result.AfterRevision {
			if err := ctx.verifyWriteSetLanded(docId, plan); err != nil {
				return plan.Result, err
			}
			return plan.Result, nil
		}

		if generation > committed {
			return plan.Result, &SupersededError{
				DocumentID:          docId,
				ExpectedRevision:    plan.Result.AfterRevision,
				ActualRevision:      docEntry.Hash,
				CommittedGeneration: committed,
				CurrentGeneration:   generation,
			}
		}

		if attempt >= len(readbackRetryDelays) {
			return plan.Result, fmt.Errorf(
				"readback: server root is at generation %d, not past the generation %d this write committed at; replica lag suspected — retry later",
				generation, committed)
		}
		time.Sleep(readbackRetryDelays[attempt])
	}
}

// verifyWriteSetLanded proves, by bytes rather than by hash chain alone,
// that the exact blobs this write produced are retrievable from the server:
// the doc index at plan.Result.AfterRevision, and the .content/.metadata
// blobs it names. Three small GETs of only the bytes this write sent — no
// root index, no ink blobs.
func (ctx *ApiCtx) verifyWriteSetLanded(docId string, plan *tagUpdatePlan) error {
	docReader, err := ctx.blobStorage.GetReader(plan.Result.AfterRevision, addExt(docId, archive.DocSchemaExt))
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}
	defer docReader.Close()
	docEntries, _, err := parseIndex(docReader)
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}

	for _, want := range []struct {
		ext  archive.RmExt
		hash string
		sent []byte
	}{
		{archive.ContentExt, plan.Result.ContentHash, plan.Content},
		{archive.MetadataExt, plan.Result.MetadataHash, plan.Metadata},
	} {
		name := addExt(docId, want.ext)
		var fileEntry *Entry
		for _, e := range docEntries {
			if e.DocumentID == name {
				fileEntry = e
				break
			}
		}
		if fileEntry == nil {
			return fmt.Errorf("readback: doc index for %s has no %s entry", docId, want.ext)
		}
		if fileEntry.Hash != want.hash {
			return fmt.Errorf("readback: doc index for %s lists %s at %s, this write produced %s", docId, name, fileEntry.Hash, want.hash)
		}

		got, err := ctx.blobStorage.GetReader(fileEntry.Hash, name)
		if err != nil {
			return fmt.Errorf("readback: %w", err)
		}
		gotBytes, err := io.ReadAll(got)
		got.Close()
		if err != nil {
			return fmt.Errorf("readback: %w", err)
		}
		if !bytes.Equal(gotBytes, want.sent) {
			return fmt.Errorf("readback: %s differs from what this write sent", name)
		}
	}
	return nil
}

// DocumentsFileTree reads your remote documents and builds a file tree
// structure to represent them
func DocumentsFileTree(tree *HashTree) *filetree.FileTreeCtx {

	documents := make([]*model.Document, 0)
	for _, d := range tree.Docs {
		//dont show deleted (already cached)
		if d.Metadata.Deleted {
			continue
		}
		doc := d.ToDocument()
		documents = append(documents, doc)
	}

	fileTree := filetree.CreateFileTreeCtx()

	for _, d := range documents {
		log.Trace.Printf("adding: %s docid: %s ", d.Name, d.ID)
		fileTree.AddDocument(d)
	}
	fileTree.FinishAdd()

	return &fileTree
}

// DocumentRevision returns docId's current cached revision hash — the value
// `settag --show` prints for use with a later --if-revision.
func (ctx *ApiCtx) DocumentRevision(docId string) (string, error) {
	d, err := ctx.hashTree.FindDoc(docId)
	if err != nil {
		return "", err
	}
	return d.Hash, nil
}

// SyncComplete notfies that somethings has changed (triggers tablet sync)
func (ctx *ApiCtx) SyncComplete() error {
	err := ctx.blobStorage.SyncComplete(ctx.hashTree.Generation)

	//sync can be called once per generation, ignore the error if nothing was changed
	if err == transport.ErrConflict {
		log.Trace.Printf("ignoring error: %v", err)
		return nil
	}

	if err != nil {
		log.Error.Printf("cannot send sync %v", err)
	}
	return nil
}

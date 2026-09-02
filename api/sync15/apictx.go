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

// UpdateDocumentTags applies a tag operation to one document with a
// stale-revision precondition, and returns a structured before/after report.
//
// Nothing in the tree is mutated until all three blobs (content, metadata,
// doc index) have uploaded: plan.apply runs last inside the closure, so a
// failed upload leaves the tree exactly as it found it.
//
// Sync's return value cannot be trusted for "did it land" on its own: after
// ten generation conflicts it gives up and returns nil. So after Sync the
// write is verified against the remote root, not the local cache — see
// verifyTagWriteLanded.
func (ctx *ApiCtx) UpdateDocumentTags(docId string, op TagOp, tags []string, expectedRevision string) (*TagUpdateResult, error) {
	var result *TagUpdateResult
	var plan *tagUpdatePlan

	err := Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		d, err := t.FindDoc(docId)
		if err != nil {
			return err
		}

		rawMetadata, err := ctx.fetchDocFile(d, archive.MetadataExt)
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

		rawContent, err := ctx.fetchDocFile(d, archive.ContentExt)
		if err != nil {
			return err
		}

		plan, err = planTagUpdate(d, rawContent, rawMetadata, op, tags, expectedRevision, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		result = plan.Result
		if !plan.Result.Changed {
			return errTagWriteNoop
		}

		if err := verifyTagSplice(rawContent, plan.Content, plan.Result.AfterTags); err != nil {
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
		return t.Rehash()
	}, true)
	if errors.Is(err, errTagWriteNoop) {
		log.Info.Println("tags already as requested; nothing to write")
		return result, nil
	}
	if err != nil {
		return nil, err
	}

	if err := ctx.verifyTagWriteLanded(docId, plan); err != nil {
		return nil, err
	}
	return result, nil
}

// fetchDocFile reads one of a document's files, by the hash its entry carries.
func (ctx *ApiCtx) fetchDocFile(d *BlobDoc, ext archive.RmExt) ([]byte, error) {
	name := addExt(d.DocumentID, ext)
	for _, f := range d.Files {
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
	return nil, fmt.Errorf("document %s has no %s file", d.DocumentID, name)
}

// verifyTagWriteLanded proves the write from the server, not the local
// cache: it reads the remote root index, finds docId's entry, and confirms
// the doc index and both edited blobs it points at match what was sent.
func (ctx *ApiCtx) verifyTagWriteLanded(docId string, plan *tagUpdatePlan) error {
	rootHash, _, err := ctx.blobStorage.GetRootIndex()
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}
	rootReader, err := ctx.blobStorage.GetReader(rootHash, addExt("root", archive.DocSchemaExt))
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}
	defer rootReader.Close()
	rootEntries, _, err := parseIndex(rootReader)
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}

	var docEntry *Entry
	for _, e := range rootEntries {
		if e.DocumentID == docId {
			docEntry = e
			break
		}
	}
	if docEntry == nil {
		return fmt.Errorf("readback: remote root does not list %s", docId)
	}
	if docEntry.Hash != plan.Result.AfterRevision {
		return fmt.Errorf(
			"tag write for %s was not committed: remote root lists revision %s, this write produced %s",
			docId, docEntry.Hash, plan.Result.AfterRevision)
	}

	docReader, err := ctx.blobStorage.GetReader(docEntry.Hash, addExt(docId, archive.DocSchemaExt))
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
			return fmt.Errorf("readback: remote doc index for %s has no %s entry", docId, want.ext)
		}
		if fileEntry.Hash != want.hash {
			return fmt.Errorf("readback: remote %s is at hash %s, this write produced %s", name, fileEntry.Hash, want.hash)
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
			return fmt.Errorf("readback: remote %s differs from what this write sent", name)
		}
	}

	d, err := ctx.hashTree.FindDoc(docId)
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}
	if d.Hash != plan.Result.AfterRevision {
		return fmt.Errorf("readback: local tree is at %s, remote at %s", d.Hash, plan.Result.AfterRevision)
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

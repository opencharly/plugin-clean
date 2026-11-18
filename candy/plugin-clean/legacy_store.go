package clean

// legacy_store.go — disposing of the PRE-RELOCATION image store (opencharly/plugin-clean#30).
//
// # The defect this exists for
//
// opencharly/sdk#556 moved the engine-neutral image store out of W3.6's data root
// (`$XDG_DATA_HOME/charly/images`) and into the ONE cache root as the named `registry` store — the
// right move, because `D-STORE-3`/`D-STORE-10` allow one store and no second layout. It moved the
// READER and the WRITER. It did not move, ingest or retire the CONTENT: the old tree stayed where it
// was, `storekit.StoreDir`/`NamedStores` resolve under the cache root only, and no retirement-record
// entry named the data-root path. Measured on this host: 311 GiB across 202 entries that no charly
// verb could name, report or reclaim — the complaint that opened this program (#21), reintroduced
// by the fix for it.
//
// # Why INGEST, and not "keep what state references and evict the rest" in place
//
// The issue's own two acceptable dispositions are "ingest the state-referenced entries" and "keep
// only the state-referenced entries and reclaim the rest". The second needs a per-entry
// reachability walk over the LEGACY layout, whose descriptors are OCI image MANIFESTS rather than
// storekit entry manifests — a third reader, and a second implementation of the image-graph walk
// that storekit owns (`walkImageGraph` is unexported). It would also be the one disposition in this
// package that DELETES artifacts on the strength of a keep set, in a category (`--cache`) that has
// no `--store` budget, no live-build guard and no refusal discipline of its own.
//
// Ingest needs no keep set at all, and that is the point: NOTHING is dropped. Every entry is
// re-published into the `registry` store — its blobs materialized (HARDLINKED when the two trees
// share a filesystem, copied when they do not, and the report says which), its descriptor re-keyed
// into the current entry shape — and the source tree is removed ONLY after every entry has been
// verified present and every reference resolves. The result is not a smaller store; it is a store
// the policy can SEE. `charly clean --store` then applies D-STORE-7's rule (keep what state
// references, evict the rest, within the authored budget) to it, in the category that owns that
// judgement, with its refusal and its live-build decline.
//
// That is also why there is no "refuse when the state surface is unreadable" step here: this
// transform never consults the state, so no state surface can make it unsafe. The refusals it DOES
// have are about the bytes it is moving, and each names itself.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencharly/sdk/storekit"
)

// legacyIndexDescriptor is one descriptor of a legacy `index.json`: the whole of what the
// pre-relocation store recorded about an entry (the reference and the build time ride in the
// annotations, exactly as the current store's entries carry them).
type legacyIndexDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// legacyIndexShape is the part of a legacy `index.json` this ingest reads.
type legacyIndexShape struct {
	SchemaVersion int                     `json:"schemaVersion"`
	MediaType     string                  `json:"mediaType,omitempty"`
	Manifests     []legacyIndexDescriptor `json:"manifests"`
}

// ingestLegacyImageStore re-publishes every entry of a pre-relocation image store into the current
// `registry` store, verifies the result, and only then removes the source tree.
//
// A dry run reads the source, fills the report and touches NOTHING — neither store nor source.
func ingestLegacyImageStore(src, dstDir string, dryRun bool, out *retiredRootOutcome) error {
	indexPath := filepath.Join(src, "index.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		// A legacy tree with blobs but no readable index is content this ingest cannot account for.
		// Removing it would delete bytes nothing recorded, so it is refused rather than swept.
		if os.IsNotExist(err) {
			return fmt.Errorf("the legacy store has no %s, so this sweep cannot account for its blobs — leaving it untouched", indexPath)
		}
		return fmt.Errorf("reading the legacy store's index %s: %w", indexPath, err)
	}
	var idx legacyIndexShape
	if uerr := json.Unmarshal(raw, &idx); uerr != nil {
		return fmt.Errorf("parsing the legacy store's index %s: %w", indexPath, uerr)
	}
	out.Bytes = treeBytes(src)
	out.Entries = len(idx.Manifests)
	if len(idx.Manifests) == 0 {
		return fmt.Errorf("the legacy store's index %s names no entries, so this sweep cannot account for the %d byte(s) beside it — leaving it untouched", indexPath, out.Bytes)
	}
	for i, d := range idx.Manifests {
		if d.Digest == "" || d.MediaType == "" {
			return fmt.Errorf("the legacy store's index entry %d carries no digest or media type — refusing to ingest an entry it cannot name", i)
		}
		if d.Annotations[storekit.AnnotationImageName] == "" {
			return fmt.Errorf("the legacy store's index entry %s carries no %s annotation, so the reference it was built as is unknown — refusing to ingest it under a name this sweep would have to invent", d.Digest, storekit.AnnotationImageName)
		}
	}
	if dryRun {
		return nil
	}

	dst := storekit.OpenStoreDir(dstDir)
	if err := dst.EnsureLayout(); err != nil {
		return fmt.Errorf("preparing the destination store %s: %w", dstDir, err)
	}
	// Every descriptor must name a blob the legacy tree ACTUALLY holds, before a single byte is moved:
	// an entry whose manifest is already missing is a tree this ingest cannot faithfully re-publish,
	// and removing it afterwards would delete the record of an image that never made it across.
	for _, d := range idx.Manifests {
		if dst.HasBlob(d.Digest) {
			continue
		}
		srcPath := filepath.Join(src, "blobs", filepath.FromSlash(strings.Replace(d.Digest, ":", "/", 1)))
		if _, serr := os.Stat(srcPath); serr != nil {
			return fmt.Errorf("the legacy store's index names %s but the tree does not hold it — leaving everything untouched", d.Digest)
		}
	}
	linked, lerr := materializeLegacyBlobs(src, dst)
	if lerr != nil {
		return lerr
	}
	out.Linked = linked

	// Re-key each legacy entry into the CURRENT entry shape: an entry's payload is the artifact
	// INDEX that names its image manifest, so a reader (and `charly clean --store`) walks it with the
	// same code every other entry uses. Copying a legacy descriptor verbatim would leave an entry
	// whose payload is a manifest, which is not the shape the store reads.
	want := make(map[string]string, len(idx.Manifests)) // entry key -> reference
	for _, d := range idx.Manifests {
		payload, perr := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     storekit.MediaTypeOCIIndex,
			"manifests":     []any{map[string]any{"mediaType": d.MediaType, "digest": d.Digest, "size": d.Size}},
		})
		if perr != nil {
			return fmt.Errorf("rendering the artifact index of %s: %w", d.Digest, perr)
		}
		key, perr := dst.PutBlob(payload)
		if perr != nil {
			return fmt.Errorf("publishing the artifact index of %s: %w", d.Digest, perr)
		}
		ref := d.Annotations[storekit.AnnotationImageName]
		annotations := map[string]string{storekit.AnnotationImageName: ref}
		if created := d.Annotations[storekit.AnnotationCreated]; created != "" {
			annotations[storekit.AnnotationCreated] = created
		}
		if _, eerr := dst.PutEntry(storekit.Entry{
			Key:         key,
			MediaType:   storekit.MediaTypeOCIIndex,
			Payload:     key,
			Annotations: annotations,
		}); eerr != nil {
			return fmt.Errorf("indexing %s in the destination store: %w", ref, eerr)
		}
		want[key] = ref
	}

	// VERIFY BEFORE REMOVING. Every entry must be in the destination index AND every reference must
	// resolve to it; a partial ingest that removed the source would be the one unrecoverable outcome
	// this transform can have.
	entries, eerr := dst.Entries()
	if eerr != nil {
		return fmt.Errorf("verifying the destination store %s: %w", dstDir, eerr)
	}
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Key] = true
	}
	for key, ref := range want {
		if !have[key] {
			return fmt.Errorf("verification failed: the destination store does not hold the entry for %s, so the source tree is NOT removed", ref)
		}
	}
	img, oerr := storekit.OpenImageStore()
	if oerr != nil {
		return fmt.Errorf("opening the destination image store for verification: %w", oerr)
	}
	for key, ref := range want {
		if dgst, ok, lerr := img.Lookup(ref); lerr != nil || !ok || dgst != key {
			return fmt.Errorf("verification failed: %s does not resolve to its ingested entry, so the source tree is NOT removed", ref)
		}
	}

	if rerr := os.RemoveAll(src); rerr != nil {
		return fmt.Errorf("the ingest verified %d entry(ies) but the source tree %s could not be removed: %w", len(want), src, rerr)
	}
	out.Ingested = true
	out.Removed = true
	return nil
}

// materializeLegacyBlobs puts every blob the legacy tree holds into the destination store, returning
// whether they were HARDLINKED (shared bytes) rather than copied.
//
// A hardlink is the honest default: both trees are content-addressed, the same digest names the same
// bytes, and a link costs no space — which matters when the tree is hundreds of GiB. Two trees on
// different filesystems cannot be linked (EXDEV), and there the bytes are copied; that is not a
// hidden fallback but the only way to move content between filesystems, it is REPORTED, and the
// source is removed afterwards either way, so the result holds one copy of each blob.
func materializeLegacyBlobs(src string, dst *storekit.Store) (linkedAll bool, err error) {
	blobsRoot := filepath.Join(src, "blobs")
	linkedAll = true
	walkErr := filepath.WalkDir(blobsRoot, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		rel, rerr := filepath.Rel(blobsRoot, path)
		if rerr != nil {
			return rerr
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 {
			return fmt.Errorf("the legacy tree holds %s, which is not a blobs/<alg>/<hex> path", path)
		}
		dgst := parts[0] + ":" + parts[1]
		dest, derr := dst.BlobPath(dgst)
		if derr != nil {
			return fmt.Errorf("the legacy tree holds %s: %w", dgst, derr)
		}
		if _, serr := os.Stat(dest); serr == nil {
			return nil // content-addressed bytes are the same bytes
		}
		if merr := os.MkdirAll(filepath.Dir(dest), 0o755); merr != nil {
			return merr
		}
		if lerr := os.Link(path, dest); lerr != nil {
			linkedAll = false
			if cerr := copyLegacyBlob(path, dest); cerr != nil {
				return fmt.Errorf("moving blob %s into the destination store: %w", dgst, cerr)
			}
		}
		return nil
	})
	if walkErr != nil {
		return false, fmt.Errorf("moving the legacy store's blobs: %w", walkErr)
	}
	return linkedAll, nil
}

// copyLegacyBlob copies one blob to a path the destination store will read. The write is staged in a
// temp file in the destination's own directory and renamed, so a reader never sees a partial blob.
func copyLegacyBlob(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // read-only open; the close error carries nothing
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, cerr := io.Copy(tmp, in); cerr != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return cerr
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return serr
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(name)
		return cerr
	}
	if rerr := os.Rename(name, dest); rerr != nil {
		_ = os.Remove(name)
		return rerr
	}
	return nil
}

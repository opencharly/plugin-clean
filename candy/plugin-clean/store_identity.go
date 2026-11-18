package clean

// store_identity.go — WHICH READER sweeps a named store, decided by WHAT THE STORE IS.
//
// # The bug this file exists to make unrepresentable
//
// `charly clean --cache` used to sweep in two halves — `spec/cache` layouts by name under the
// CONFIG root, and `storekit` stores by name under the CACHE root — and it SKIPPED the second half
// whenever the two roots resolved to the same directory, a comparison of resolved paths whose own
// comment called the coincidence an env-var case. It is not: with `D-STORE-1`'s one resolver the
// two roots ARE one directory, always. So on every such host the
// storekit-written `registry` store (opencharly/sdk#556) was handed to `spec/cache`'s
// `GCStats`, whose reachability walk knows ONE shape — a `spec/cache` entry manifest with a
// config and a layers list. A storekit entry manifest has neither, so the walk marked the
// entry's own payload live and EVERYTHING the payload reaches unreferenced: the image
// manifest, its config and its layers were deleted while the entry stayed, leaving a store
// that still answered `Lookup(ref) = (digest, true)` for content that no longer existed
// (opencharly/plugin-clean#27). It also wrote `spec/cache`'s `.lock` INSIDE the store tree,
// which `D-STORE-3` (DESIGN.md:2484) forbids.
//
// # The rule
//
// A store's READER is a property of the STORE, not of the directory it happens to sit under.
// The store declares it: an OCI layout's `index.json` descriptors carry the media type of the
// entry manifests they point at. Every descriptor of a storekit store is
// `application/vnd.opencharly.store.entry.v1+json`; every descriptor of a `spec/cache` layout
// is an OCI image manifest. That is the identity test below, and the consequence is that the
// two enumerations become ONE inventory (deduped by resolved directory) in which no store is
// ever skipped and no store is ever handed to a reader that cannot read it.
//
// A layout whose identity cannot be decided — a mixed index, or a descriptor media type this
// package has never seen — is REFUSED: it is reported by name and left exactly as it is. An
// unreadable store is not an empty one, and a sweep that guessed would be the same class of
// defect one shape over.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencharly/sdk/storekit"
	"github.com/opencharly/spec/cache"
)

// The two descriptor media types that DECIDE a store's reader. `storekit.MediaTypeStoreEntry` is
// the store's own exported constant; the OCI image manifest type has no exported counterpart in
// storekit (the package names it privately because it must never be written as an entry's media
// type, only parsed), and a `spec/cache` layout's descriptors are exactly OCI image manifests
// (`spec/cache` writes `ociv1.MediaTypeImageManifest`).
const (
	mediaTypeOCIimageManifest = "application/vnd.oci.image.manifest.v1+json"

	// indexFileName is the OCI image layout's entry point (`index.json`). storekit names it
	// privately because it reads the file through its own path builder; the identity test
	// reads it directly, which is the one thing this file does that a store's own reader
	// cannot answer for it.
	indexFileName = "index.json"
)

// storeReader names the ONE resolver allowed to sweep a store.
type storeReader int

const (
	// storeReaderStorekit — index descriptors are storekit ENTRY manifests, so the store's
	// entries are `storekit.Entry` values and `storekit`'s own reachability walk (which
	// follows image indexes THROUGH to their config and layers) is the only correct reader.
	storeReaderStorekit storeReader = iota
	// storeReaderSpecCache — index descriptors are `spec/cache` entry manifests (one payload
	// layer plus a validity config), so `Layout.GCStats` is the reader that knows the shape.
	storeReaderSpecCache
	// storeReaderUnrouted — the identity cannot be decided. Never swept; reported.
	storeReaderUnrouted
)

// String is the reader's user-visible name, for a refusal line.
func (r storeReader) String() string {
	switch r {
	case storeReaderStorekit:
		return "storekit"
	case storeReaderSpecCache:
		return "spec/cache"
	default:
		return "unrouted"
	}
}

// discoveredStore is ONE layout on disk, with the reader its own index selects.
type discoveredStore struct {
	// Name is the store's directory name under the root it was found in (`registry`, `project`).
	Name string
	// Dir is the layout's absolute directory.
	Dir string
	// Reader is the ONE resolver that may sweep it; storeReaderUnrouted means nobody may.
	Reader storeReader
	// Reason explains an unrouted store (empty for a routed one).
	Reason string
}

// storeIndexDescriptor is the subset of an OCI index descriptor this file reads to decide a
// store's identity. It is deliberately NARROW: the identity question is the media type, and a
// reader that decoded the entry bodies to answer it would be a reader in its own right.
type storeIndexDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

// storeIndexShape is the part of `index.json` the identity test needs.
type storeIndexShape struct {
	Manifests []storeIndexDescriptor `json:"manifests"`
}

// classifyStore decides which reader may sweep the layout at dir.
//
// The three answers are exhaustive and each is reached by EVIDENCE from the store, never by a
// name or a path: every descriptor an entry manifest → storekit; every descriptor an image
// manifest → spec/cache; anything else (a mixture, an unknown type, an index that cannot be
// read) → unrouted, with the evidence in the reason. An ABSENT index is an empty store, not an
// unreadable one: either reader's sweep over it is a no-op, so it is routed to storekit (whose
// walk is the stricter of the two) rather than refused.
func classifyStore(dir string) (storeReader, string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return storeReaderStorekit, "", nil
		}
		return storeReaderUnrouted, fmt.Sprintf("its index cannot be read: %v", err), nil
	}
	var idx storeIndexShape
	if uerr := json.Unmarshal(raw, &idx); uerr != nil {
		return storeReaderUnrouted, fmt.Sprintf("its index does not parse: %v", uerr), nil
	}
	if len(idx.Manifests) == 0 {
		return storeReaderStorekit, "", nil
	}
	entryManifests, imageManifests := 0, 0
	unknown := map[string]int{}
	for _, d := range idx.Manifests {
		switch d.MediaType {
		case storekit.MediaTypeStoreEntry:
			entryManifests++
		case mediaTypeOCIimageManifest:
			imageManifests++
		default:
			unknown[d.MediaType]++
		}
	}
	switch {
	case entryManifests == len(idx.Manifests):
		return storeReaderStorekit, "", nil
	case imageManifests == len(idx.Manifests):
		return storeReaderSpecCache, "", nil
	case len(unknown) > 0:
		return storeReaderUnrouted, fmt.Sprintf(
			"its index mixes descriptor media types this sweep has no reader for (%s); sweeping it with either reader could delete live bytes",
			describeMediaTypes(unknown)), nil
	default:
		return storeReaderUnrouted, fmt.Sprintf(
			"its index mixes storekit entry manifests (%d) with spec/cache manifests (%d), so neither reader knows the whole store",
			entryManifests, imageManifests), nil
	}
}

// describeMediaTypes renders a media-type histogram deterministically, so a refusal line is
// stable across runs and diffable in a test.
func describeMediaTypes(counts map[string]int) string {
	type row struct {
		name string
		n    int
	}
	rows := make([]row, 0, len(counts))
	for name, n := range counts {
		if name == "" {
			name = "(absent)"
		}
		rows = append(rows, row{name: name, n: n})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s x%d", r.name, r.n))
	}
	return strings.Join(parts, ", ")
}

// discoverStores builds the ONE inventory of named stores this host has, over BOTH roots, and
// routes each by identity.
//
// The enumeration itself stays where it already lives — `cache.NamedStores()` and
// `storekit.NamedStores()` each own the rule for "what is a store under my root" — and this
// function only JOINS the two answers, dedupes by RESOLVED DIRECTORY (the roots coincide
// whenever `CHARLY_CACHE_DIR` is set, which is the case that used to skip a half rather than
// merge it), and classifies.
func discoverStores() ([]discoveredStore, error) {
	type rootStores struct {
		label string
		names []string
		root  string
	}
	var roots []rootStores

	if root, err := cache.StoreRoot(); err != nil {
		return nil, fmt.Errorf("the cache root: %w", err)
	} else if names, nerr := cache.NamedStores(); nerr != nil {
		return nil, fmt.Errorf("the spec/cache stores: %w", nerr)
	} else {
		roots = append(roots, rootStores{label: "spec/cache", names: names, root: root})
	}
	if root, err := storekit.CacheDir(); err != nil {
		return nil, fmt.Errorf("the storekit cache root: %w", err)
	} else if names, nerr := storekit.NamedStores(); nerr != nil {
		return nil, fmt.Errorf("the storekit stores: %w", nerr)
	} else {
		roots = append(roots, rootStores{label: "storekit", names: names, root: root})
	}

	seen := map[string]bool{}
	var out []discoveredStore
	for _, r := range roots {
		for _, name := range r.names {
			dir := filepath.Join(r.root, name)
			key, aerr := filepath.Abs(dir)
			if aerr != nil {
				return nil, fmt.Errorf("resolving the store dir %s: %w", dir, aerr)
			}
			key = filepath.Clean(key)
			if seen[key] {
				continue
			}
			seen[key] = true
			reader, reason, cerr := classifyStore(key)
			if cerr != nil {
				return nil, fmt.Errorf("classifying the store %s: %w", key, cerr)
			}
			out = append(out, discoveredStore{Name: name, Dir: key, Reader: reader, Reason: reason})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Dir < out[j].Dir
	})
	return out, nil
}

// sweepStoreBlobs frees every blob no entry reaches in ONE storekit store — the destructive half
// that belongs with the surface that also reports it, never inside a discovery walk.
//
// It is ONE implementation with TWO callers, because the same question ("which bytes does this
// store no longer need?") has two askers: the `cache` category sweeps every named store, and the
// `store` category sweeps the image store after its entry eviction. A second copy of this loop
// would be the drift this package has already paid for once.
func sweepStoreBlobs(s *storekit.Store, dryRun bool) (blobs int64, bytes int64, err error) {
	cands, cerr := s.UnreferencedBlobs()
	if cerr != nil {
		return 0, 0, cerr
	}
	for _, c := range cands {
		// The report counts what the sweep RECLAIMED on a real run and what it WOULD reclaim on
		// a dry one, so `--dry-run` and the real run agree on the number.
		if !dryRun {
			if rerr := s.RemoveBlob(c.Digest); rerr != nil {
				return blobs, bytes, fmt.Errorf("remove %s: %w", c.Digest, rerr)
			}
		}
		blobs++
	}
	return blobs, storekit.BlobBytes(cands), nil
}

// refusedStore is ONE named store the sweep declined to touch: its identity could not be decided
// from its own index, so no reader could be trusted with it. It rides beside the wire reply (like
// the skips and the retired roots) because `spec.CacheStoreInfo` is a contract type in
// opencharly/spec and a refusal is this repository's own report line.
type refusedStore struct {
	Name   string
	Dir    string
	Reason string
}

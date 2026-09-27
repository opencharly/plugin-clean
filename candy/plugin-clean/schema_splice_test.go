package clean

import (
	"context"
	"strings"
	"testing"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"

	pb "github.com/opencharly/spec/proto"
	sdkschema "github.com/opencharly/spec/schema"
	"github.com/opencharly/spec/schemaconcat"
)

// TestServedSchemaSplicesOntoBase reproduces charly's host schema load gate
// (charly/charly/plugin_loader.go compileBasePlusServed) IN THIS MODULE: it
// concatenates the REAL base schema (`github.com/opencharly/spec/schema`'s
// embedded FS — the same FS charly uses) with this plugin's served schema and
// compiles `base ++ plugin` with the same cuecontext.New() the kernel uses.
//
// It FAILS without this change: before it, NewMeta passed a nil schema FS, so the
// served schema was empty and #CleanPlugin was undefined — the host gate would reject the
// unit ("schema does not splice onto the base").
func TestServedSchemaSplicesOntoBase(t *testing.T) {
	caps, err := NewMeta().Describe(context.Background(), &pb.Empty{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	served := caps.GetSchemaCue()
	if strings.TrimSpace(served) == "" {
		t.Fatal("Describe served an EMPTY schema_cue; there is no schema-less plugin")
	}

	base, _, err := schemaconcat.ConcatSchema(sdkschema.FS, ".", nil)
	if err != nil {
		t.Fatalf("read base schema: %v", err)
	}

	merged := cuecontext.New().CompileString(base + "\n" + served)
	if err := merged.Err(); err != nil {
		t.Fatalf("served schema does not splice onto the base (base ++ plugin): %v", err)
	}
	def := merged.LookupPath(cue.ParsePath("#CleanPlugin"))
	if def.Err() != nil {
		t.Fatalf("base ++ plugin does not define #CleanPlugin: %v", def.Err())
	}
	if _, err := def.Struct(); err != nil {
		t.Fatalf("#CleanPlugin is not a struct after the splice: %v", err)
	}
}

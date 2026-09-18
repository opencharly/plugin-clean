# plugin-clean

The `plugin-clean` plugin candy of the [opencharly/charly](https://github.com/opencharly/charly)
candy library, as a standalone repo (the candy de-submodule cutover, plugin
kind). The Go module lives at `candy/plugin-clean/` with module path
`github.com/opencharly/plugin-clean/candy/plugin-clean`; the charly resolver fetches this repo at the pinned tag and
the compiled-in wiring imports the module at that path.

## Platforms

Builds for `linux/amd64`, `linux/arm64` and `linux/arm/v7`. The armv7 target
works because of the sdk's 32-bit fix
([opencharly/sdk#263](https://github.com/opencharly/sdk/pull/263), issue
[#262](https://github.com/opencharly/sdk/issues/262)) — `charly`'s loader
host-builds this plugin with `CGO_ENABLED=0`, so the artifact is a static
binary, which is what an armv7 appliance without a glibc toolchain (such as a
JetKVM's uClibc userland) runs. Nothing extra is needed to use it: install
`charly` and it builds the plugin for the host it runs on.

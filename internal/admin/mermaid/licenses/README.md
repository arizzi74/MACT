# Licenses for dependencies prebundled by Mermaid

Mermaid's published `dist` chunks contain code from these dependencies, identified
by the exact package versions in the accompanying source maps:

- `fastdom@1.0.12`: license section copied from `package/README.md` in the npm
  `fastdom-1.0.12.tgz` package (SHA-1 `ae43d55af017252ae499b2e186511ab97412de39`).
- `js-yaml@4.3.0`: `package/LICENSE` copied from the npm `js-yaml-4.3.0.tgz`
  package (SHA-1 `d1900572a7f7cf0b5f540c83673e60bad3436592`).

Mermaid 12.2.0 still contains the js-yaml 4.3.0 chunk, but our build replaces
that dedicated chunk with the compatible patched npm package, js-yaml 4.3.2.
The replacement verifies the original chunk hash and its application count.
The shipped notices and inventory therefore include 4.3.2's installed license;
the historical 4.3.0 notice here is retained for the upstream distribution.

`scripts/build-mermaid.mjs` includes these notices together with the license
files of every package contributing code to the renderer bundle. It verifies
that every prebundled package found in Mermaid's source maps has a notice; a
future Mermaid update introducing another prebundled dependency fails the build
until its notice is included. The generated, complete notice is
`internal/admin/static/webui-mermaid-LICENSE.txt`.

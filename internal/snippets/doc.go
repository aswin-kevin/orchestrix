// Package snippets compiles the Go code blocks of README.md, llms.txt and
// llms-full.txt, so the documentation cannot drift from the code. The core
// module's internal/doccheck test and a test in each satellite that has
// dependencies of its own (orxotel, orxpg) call Check for their share.
//
// Every block fenced as ```go becomes one package of a scratch module,
// wrapped by these rules:
//
//   - A block that starts with a package clause is a complete file.
//   - A block that holds an import declaration is a file without its
//     package clause; one is added. When it declares a func Test it is
//     written as a _test.go file.
//   - Any other block is a fragment with its imports omitted. Its leading
//     type, func, var and const declarations stay at package level and the
//     statements after them become the body of a function, which returns
//     error when one of the statements returns a value. Each variable the
//     body declares at its top level is used once (_ = v) right after it,
//     so a fragment may end without using what it declares.
//   - A name a fragment uses but does not declare is resolved in this order:
//     the predeclared identifiers, the declarations in freeNames (variables
//     such as ctx that a fragment says come from surrounding code), the
//     packages of the repository (a name shared by several packages, such as
//     memory, picks the one that exports every name the fragment selects
//     from it), then stdPackages and thirdParty. Any other name fails the
//     check: declare it in the block or add it to freeNames.
//
// A block belongs to the module whose test compiles it: orxotel when it
// imports orxotel or OpenTelemetry, orxpg when it imports orxpg or pgx,
// and the core module otherwise (orxdocker needs nothing beyond the core,
// so the core test compiles its blocks). The scratch module requires the
// core and that satellite through replace directives to the working tree,
// uses the requirements and go.sum of the module whose test runs, and is
// checked with go vet. It loads the module graph that module's own build
// uses, so with a warm module cache it needs no network; on a cold cache
// the go command fetches what is missing through GOPROXY, as go test does.
// Check skips in short mode and outside a repository checkout.
package snippets

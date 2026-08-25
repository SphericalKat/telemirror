# Embed aria2go as the download engine

Telemirror will embed `aria2go` as an in-process Go download engine instead of starting `aria2c` or an `aria2go` child process.
Telemirror will extend or maintain a fork of `aria2go` when its public API lacks the per-download status, file, cancellation, and completion operations required by compatibility-first behavior.

The fork source is `https://github.com/smartass08/aria2go`.
Telemirror's fork is `https://github.com/SphericalKat/aria2go-1`.

The aria2go author has granted written permission to distribute the used code under MIT-compatible terms.
Telemirror will remain MIT-licensed and will retain a record of that permission outside source code as needed.

This keeps the service Go-native and avoids a process or RPC boundary, at the cost of maintaining the required `aria2go` API surface.

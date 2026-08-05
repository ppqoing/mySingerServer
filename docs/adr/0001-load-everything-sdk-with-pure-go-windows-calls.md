# Load Everything SDK with pure-Go Windows calls

Status: accepted

`agent.exe` loads `Everything64.dll` on demand through `golang.org/x/sys/windows` and resolves the Everything 1.4 SDK functions dynamically. This preserves the optional runtime DLL boundary and Walker fallback while avoiding a cgo ABI shim and a MinGW requirement in the Agent build.

The M1 Agent is therefore built with `CGO_ENABLED=0` for `windows/amd64`; `Everything64.dll` is still distributed beside it. A future `worker.exe` may independently require cgo for `mediacore.dll`, but that does not change the Agent decision.

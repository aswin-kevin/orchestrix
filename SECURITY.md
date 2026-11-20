# Security policy

## Reporting a vulnerability

Please report security issues privately, through GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a public issue, pull request or discussion for a suspected vulnerability.

Include what you can of the following: the affected package and version (or commit), a description of the issue and its impact, and steps or a program that reproduce it. You will get an acknowledgement, and the fix is coordinated with you through the private advisory, which is published once a fixed release is out.

## Scope

Orchestrix runs model-chosen actions on the host, so the parts that stand between a model and the host matter most. Reports about these are especially welcome:

- **The permission parser** (`permission`): a shell line that an allow rule vouches for but that runs a command the parser did not list, or a deny or ask rule that a line can evade. A rule such as `execute(git:*)` is meant for git's porcelain; git's plumbing and transport commands (`fetch-pack`, `send-pack`, `upload-pack`, `remote-ext` and the like) are never vouched for, by design.
- **Backends** (`backend/...`): a path that escapes a disk, shell, key/value or composite backend's root, or a workspace that one thread or threadless run can read from another.
- **The sandbox backend** (`backend/sandbox`): quoting or encoding mistakes that let file operations run other commands in the sandbox.
- **orxdocker**: a container setting that weakens the documented hardening, a command that escapes its time limit or container, or a container shared between threads.
- **The MCP server** (`mcp.Server` and its HTTP handler): bypasses of the host and origin checks, or session handling that lets one client act in another's session.
- **The web_fetch network guard** (`middleware/web`): a URL, redirect or DNS answer that reaches a loopback, private, link-local or other non-public address while the guard is on.

Also in scope: credentials leaking into errors, logs or traces (API keys, sensitive headers, URL passwords and query values), and memory or CPU exhaustion from model, server or web content in places that document a bound.

Out of scope: what a model chooses to do within the permissions you granted it, behaviour that the documentation describes as not a sandbox (the shell backend runs commands as your process), and issues in a model provider's service.

## Supported versions

Orchestrix is pre-1.0. Security fixes go into the latest minor release only (the newest `v0.x` core release, and the newest release of each satellite module: `orxotel`, `orxpg`, `orxdocker`). Upgrade to it to receive fixes.

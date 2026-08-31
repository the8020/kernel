# Purpose

- Share all metadata-driven command UI behavior across administrative modes.

# Ownership

- Own path/alias matching, positional and long-option conversion, help, local errors, exit mapping, text/JSON rendering, and interactive line tokenization.
- Do not own console lifecycle, socket paths, transport implementation, or command-specific branches.

# Local Contracts

- Public API: `Executor`, `ValueResolver`, `SecretResolver`, `Runner`, `New`,
  `Runner.SetValueResolver`, `Runner.SetSecretResolver`, `Runner.Run`,
  `Runner.Help`, and `SplitLine`.
- Primary paths and aliases resolve from the generated catalog; positionals, `--name value`, `--name=value`, boolean flags, and the `--` terminator are converted before the client request.
- An omitted required positional with a metadata-declared ordinary prompt is
  resolved and type-checked before any secret prompt; other missing parameters
  remain local errors. Ordinary password arguments are rejected, secure
  prompts request confirmation, and automation must opt into the declared
  standard-input flag.
- `help` is local; its global view lists catalog commands plus the local `help` and interactive `exit` commands as normal command rows, and both local commands have detailed help topics.
- Catalog-matched operations use the typed executor; `admin` owns execution of the local `exit` command.
- A single-result array of `{key, description}` summaries renders compactly as unlabeled key and indented description rows.
- Every other single-result array of flat scalar summaries renders as unlabeled resource blocks separated by one blank line, with the resource ID first and domain fields in stable readable order; an empty collection renders as `<collection>: none`.
- Detailed objects render identity, description, declared settings storage,
  canonical location, configured/active/default values, overrides, source, and
  runtime state in stable preferred order before alphabetically ordered unknown
  fields.
- Named `core.Result` maps render directly instead of passing through lossy JSON normalization; execution summaries render state, result, logs, duration, resources, and execution identity in that order when present.
- Extend syntax only through command metadata or shared client-local behavior.

# Work Guidance

- Keep one parser and renderer for one-shot and interactive modes.

# Verification

- `cli_test.go` covers aliases, typed and metadata-prompted arguments, prompt
  ordering, complete global help, local and catalog help topics, examples,
  errors, compact setting and flat resource summaries, empty collections,
  detailed rendering order, exact large-integer rendering, and quoted input.

# Child DOX Index

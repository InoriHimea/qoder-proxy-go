// cli_wrapper.mjs — bypasses Linux ARG_MAX by loading the CLI in-process.
//
// When the system prompt is too large for command-line args (> ~100KB),
// the Go proxy writes it to a temp file and spawns this wrapper instead
// of the CLI directly.  The wrapper reads the prompt from the file,
// injects it into process.argv, and dynamically imports the CLI bundle
// — all in-process, so no exec/execve call is made with the large payload.
//
// Usage:
//   node cli_wrapper.mjs <cli_js_path> <sysprompt_file> [cli_args...]
//
// The wrapper preserves stdin/stdout/stderr (inherited from the parent
// Go process) so the CLI's stream-json output and stdin prompt piping
// work identically to a direct spawn.

import { readFileSync } from 'fs';

const cliPath   = process.argv[2];  // e.g. /usr/local/lib/node_modules/@qodercn-ai/qoderclicn/bundle/qoderclicn.js
const spFile    = process.argv[3];  // temp file containing the system prompt
const cliArgs   = process.argv.slice(4);  // remaining CLI flags (-p, -, -f, stream-json, --model, ...)

// Read system prompt from file (can be arbitrarily large — no ARG_MAX)
const sysPrompt = readFileSync(spFile, 'utf8');

// Reconstruct process.argv exactly as the CLI expects:
//   argv[0] = node binary
//   argv[1] = CLI script path (the CLI uses this for __dirname / import.meta.url)
//   argv[2..] = CLI flags + --system-prompt <content>
process.argv = [process.argv[0], cliPath, ...cliArgs, '--system-prompt', sysPrompt];

// Execute the CLI in-process.  Because we use dynamic import() rather than
// a shell or exec(), the OS never sees the large system prompt as an
// argument — it exists only in V8 heap memory.
await import(cliPath);

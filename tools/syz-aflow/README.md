# `syz-aflow` - AI Workflow Executor

`syz-aflow` is a CLI tool designed to execute `aflow` workflows locally for debugging and testing purposes.

## Building

To build `syz-aflow`, use the `syz-env` environment from the repository root:
```bash
./tools/syz-env go build ./tools/syz-aflow
```
This will create a `syz-aflow` binary in the repository root.

## Usage

### Basic Execution

To run a workflow, you need to specify the workflow name, an input JSON file, and a working directory:

```bash
./tools/syz-env ./syz-aflow -workflow <workflow_name> -input <input.json> -workdir <workdir>
```

### Batch Execution

If `-input` is a directory, each `*.json` file in it is executed as a separate task:

```bash
./tools/syz-env ./syz-aflow -workflow seed-gen-file-line -input ./tasks -workdir ./workdir -parallel 4
```

Results are saved to `<workdir>/trajectories/<state>/`. Completed tasks are skipped on restart.
Batch execution is currently only supported for the `seed-gen-file-line` workflow and cannot be combined
with `-html` and `-output`.

To spread the load over several Gemini API keys, put them one per line into `GOOGLE_API_KEY`
(e.g. `GOOGLE_API_KEY="$(cat keys.txt)"`). Each task sticks to one key. Keys only help with rate limits
if they belong to different projects.

With `-corpus <path>`, all programs executed during the batch run are also saved into the given `corpus.db`
(without call properties such as fault injection), e.g. to be used as fuzzing seeds.

### Workflow Inputs

`syz-aflow` does not require a standard `syz-manager` configuration file. Instead, it takes a JSON file containing the arguments specific to the workflow you are running.

If the workflow needs to perform actions that interact with VMs (like reproducing a crash or testing a patch), it will expect fields in that input JSON that describe the environment (e.g., `Image`, `VM` type and config, `KernelSrc`, etc.). The workflow code then takes these individual arguments and builds the necessary manager configuration programmatically on the fly.

#### Example Input for `patching` Workflow

```json
{
  "Syzkaller": "/path/to/syzkaller",
  "Image": "/path/to/linux/image",
  "Type": "qemu",
  "VM": {
    "count": 1,
    "cpu": 2,
    "mem": 2048,
    "qemu_args": "-machine q35 -enable-kvm -smp 2,sockets=2,cores=1"
  },
  "ReproSyz": "syz_open$dir(0x0, 0x1) ...",
  "KernelConfig": "@/path/to/linux.config"
}
```

Any string field in the input JSON starting with `@` (e.g. `"@/path/to/file"` or `"@./relative/path"`) will be automatically expanded with the contents of that file. Relative paths are resolved relative to the directory of the `-input` JSON file. Literal leading `@` characters can be escaped with `@@` (e.g. `"@@literal"`).


### Flags

- `-workflow`: The name of the workflow to execute.
- `-input`: Path to a JSON file containing the arguments for the workflow (or a directory of them).
- `-parallel`: Number of tasks to run concurrently in batch execution.
- `-corpus`: Path to a `corpus.db` to save all executed programs into in batch execution.
- `-workdir`: Directory where the workflow can perform checkouts, builds, etc.
- `-html`: Path to an HTML file where the execution trajectory will be rendered in real-time.
- `-model`: Override the default LLM model.
- `-cache-size`: Set the maximum cache size (default "10GB").
- `-download-bug`: Download bug details from the dashboard by ID or ExtID.
- `-auth`: Use gcloud auth token when downloading bugs.

### Live Trajectory Visualization

You can monitor the execution of the workflow in real-time by using the `-html` flag:
```bash
./tools/syz-env ./syz-aflow -workflow patching -input input.json -workdir ./workdir -html trajectory.html
```
Open the specified HTML file in your browser to see the charts and steps as they execute.

### OpenAI-compatible Chat Completions

Select `-provider openai` and an explicit `-model`. Set `OPENAI_BASE_URL` to the API root
(including its path prefix, such as `/v1`); the provider appends `/chat/completions`.
Alternatively, `-openai-base-url` overrides the environment variable. `OPENAI_API_KEY`,
when nonempty, supplies Bearer authentication. Leave it unset for unauthenticated local services.

For an existing seedgen task:

```bash
OPENAI_BASE_URL=https://example.invalid/v1 ./syz-aflow \
  -provider openai -model your-model \
  -workflow seed-gen-file-line -input task.json -workdir ./workdir-openai
```

This command runs the workflow, including its kernel preparation and VM execution;
provider initialization itself does not contact the endpoint. A provider connection
does not establish that the generated program reaches the target: use the workflow's
coverage checks and independent replay.

OpenAI-specific options:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-openai-base-url` | `OPENAI_BASE_URL` | API root; required, with no public-service fallback |
| `-openai-request-timeout` | `10m` | Timeout for one model request |
| `-openai-max-output-tokens` | `16384` | Requested output limit |
| `-openai-output-token-field` | `max_tokens` | Select `max_tokens` or `max_completion_tokens` |

The same model handles all aflow model categories. Requests are non-streaming and use
native function tools. Structured outputs use aflow's existing `set-results` tool.
Services must support tool-call IDs and return `prompt_tokens` and `completion_tokens`;
missing usage is an error. Reasoning-specific options and Responses API are not supported.
Reported completion tokens already include any reasoning tokens, which are not counted again.

The provider does not retry internally: transient errors are returned to aflow's existing
retry loop. Authentication, recognized permanent quota errors, and invalid model/options
fail explicitly. `-token-limit` accounts for successful response usage, including cached
responses, and is not an exact billing cap for failed or retried requests.

LLM cache keys include a credential-free provider identity, separating endpoints and
generation settings. Use a new workdir when the service changes the model behind an
unchanged name/address. This also separates completed batch-task records.

The implementation is isolated in `pkg/aflow/backend/openai` and its CLI registration file.
The only agent extension is the optional backend cache identity. Local tests require
neither credentials nor a model service:

```bash
go test ./pkg/aflow/backend/... ./pkg/aflow ./tools/syz-aflow
```

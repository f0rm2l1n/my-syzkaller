# Runtime-guided experiment workflow

`runtime-guided-experiment` is the execution data plane for the optional `+1`
experiment plane in the N+1 architecture. It executes a supplied syzlang
scenario in an isolated `RunnerManager`, collects runtime evidence, and
classifies structured `LLMEXP` events against an explicit predicate.

The long-term output is a **semantic experiment package** and, after replay
and policy checks, a **semantic corpus entry**. A semantic corpus entry keeps
the syz program together with its preconditions, target state or event
predicate, observation plan, evidence, environment identity, and replay
status. It can later be submitted through the candidate gateway to an
ordinary N-side syz-manager instance for mutation and crash discovery.

N remains authoritative. This workflow does not write a manager `corpus.db`,
enter the fuzzer mutation hot path, or decide whether a candidate is a bug.
The current implementation is deterministic and expects `ProgramSyz`; the
LLM hypothesis loop, N snapshot import, independent replay, and candidate
gateway are subsequent layers over this data plane.

The first staging topology is one manager container paired with one
capability-2 sidecar (`N=1`). They may share a physical host and private
network for simplicity, but must use separate workdirs, `corpus.db`, VM pools,
and caches. A sidecar failure must not corrupt or stop the manager. Scaling one
experiment plane across multiple managers is a later stage.

With several existing manager containers, keep the first rollout as independent
1:1 pairs (`manager-1` with `capability-2-1`, and so on). This simplifies
identity and permissions at the cost of duplicated experiments and resources;
a centralized +1 service comes only after one pair has stable artifact,
activation, and recovery behavior.

It can be driven locally by `syz-aflow`. An eventual SuperAgent should call
the same data plane through a versioned asynchronous MCP adapter rather than
reimplementing VM or executor control. The planned MCP surface is high level:
snapshot creation/reading, experiment start/cancel, evidence retrieval, and
candidate submission through a policy gateway. Direct shell, arbitrary QEMU,
and direct `corpus.db` mutation are outside the contract.

## Standalone workflow

The canonical first-stage workflow is
`runtime-guided-experiment-standalone`. It does not connect to a resident
`syz-manager`. With `UseLLM=false`, it executes the supplied `ProgramSyz` and
then performs host-controlled replay. With `UseLLM=true`, a bounded aflow agent
may submit typed proposals through `submit-experiment-proposal`; the host
validates and executes every proposal before returning evidence.

The model never supplies the authoritative verdict, execution identity,
coverage, console, or replay result. Those fields are populated by the host
and written to a versioned package under
`<workdir>/semantic-experiments/<package-id>/package.json`. Malformed or
unsupported proposals are returned as tool errors and cannot start a VM.

The replay count is configurable through `ReplayCount` and defaults to three.
The first Linux acceptance profile uses the same `representative.syz` scenario
with the normal fixture behavior and with the fixture's controlled violation
mode; these are profile-specific oracle checks, not a general capability-2
classification.

Example deterministic input (paths are expanded by `syz-aflow` in the usual
way):

```json
{
  "AgentName": "semantic-standalone",
  "KernelRepo": "https://kernel.googlesource.com/pub/scm/linux/kernel/git/stable/linux-stable",
  "KernelCommit": "988117cc1bb17338c586a4e21ade88b5397eac33",
  "KernelConfig": "@/path/to/.config",
  "Image": "/path/to/rootfs.ext4",
  "Type": "qemu",
  "VM": {"count": 1, "cpu": 2, "mem": 2048,
         "qemu_args": "-enable-kvm -smp 2,sockets=1,cores=2"},
  "Syzkaller": "/path/to/my-syzkaller",
  "SyzkallerCommit": "standalone-run-revision",
  "TargetOS": "linux",
  "TargetArch": "amd64",
  "Snapshot": true,
  "Driver": "llm_async_demo",
  "ProgramSyz": "@representative.syz",
  "PredicateKind": "invariant",
  "PredicateVersion": "async-lifecycle-v1",
  "PredicateExpression": "callback_exit occurs before release",
  "ExpectedVerdict": "falsified",
  "ReplayCount": 3,
  "UseLLM": false
}
```

Run it with:

```sh
./tools/syz-env ./syz-aflow \
  -workflow runtime-guided-experiment-standalone \
  -input standalone.json \
  -workdir ./standalone-workdir \
  -output ./standalone-output.json
```

The first acceptance does not submit a candidate to an ordinary fuzzing
manager. It only signs off executable evidence, oracle classification,
independent replay and an auditable package.

The first fixture is `testdata/llm_async_demo`. It needs a fixture-specific
syzkaller description because the custom ioctl calls are not part of upstream
Linux descriptions. Copy `llm_async_demo.txt` into that syzkaller checkout's
`sys/linux/`, run `make descriptions`, and build `syz-aflow` from that checkout.
The representative program uses pointer syntax for the path and input buffer;
keep that syntax when editing the scenario.

Build the module against the exact `KernelObj` used to produce the booted
kernel. Use its symbol versions and signing key:

```sh
make -C "$KERNEL_OBJ" M="$PWD" LLVM=1 CONFIG_BASIC_MODVERSIONS=y modules
"$KERNEL_OBJ/scripts/sign-file" sha256 \
  "$KERNEL_OBJ/certs/signing_key.pem" \
  "$KERNEL_OBJ/certs/signing_key.x509" \
  "$PWD/llm_async_demo.ko"
```

Install the resulting `llm_async_demo.ko` and a modules-load entry into a
private copy of the rootfs. Load it only in the experiment VM. The original
kernel, rootfs, and fuzzing manager remain unchanged.

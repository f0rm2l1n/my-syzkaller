# Runtime-guided experiment workflow

The `runtime-guided-experiment` flow executes one supplied syzlang program in
an isolated `RunnerManager` and classifies structured `LLMEXP` events against
an explicit lifecycle hypothesis. It is deliberately evidence-first: missing
events produce `insufficient-evidence`, and probe output never becomes a
confirmed kernel result by itself.

The first fixture is `testdata/llm_async_demo`. Copy the fixture into the
private Linux kernel tree, build it as a module against the prepared kernel
objects, and load it only in the experiment VM. The original kernel and its
fuzzing manager remain unchanged.

Example input fields are documented by the Go `Inputs` type. The minimum
experiment-specific fields are `Driver`, `ProgramSyz`, `Hypothesis`,
`Falsification`, and `Scenario`; the remaining target fields follow the
existing seed-generation workflows.

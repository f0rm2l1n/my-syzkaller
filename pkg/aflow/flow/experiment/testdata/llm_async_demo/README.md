# `llm_async_demo` fixture

This fixture is used by the runtime-guided experiment workflow. Copy it into
the fixed Linux fixture tree and build it against that tree's prepared kernel
objects:

```sh
make -C "$KERNEL_OBJ" M="$PWD" modules
```

Load it only in the isolated experiment VM. `buggy_release=1` deliberately
emits `release` before `callback_exit` while still waiting before freeing the
object; this calibrates the oracle and is not a kernel vulnerability result.

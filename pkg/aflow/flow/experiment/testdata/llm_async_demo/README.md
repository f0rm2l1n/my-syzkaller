# `llm_async_demo` fixture

This fixture is used by the runtime-guided experiment workflow. Copy
`llm_async_demo.txt` into the syzkaller checkout used for the run, then run
`make descriptions` there. The generated syzkaller binaries must include that
description; a normal upstream binary cannot deserialize `representative.syz`.

Build the module against the exact `KernelObj` used to produce the booted kernel.
The module must use that tree's symbol versions and signing key; do not compile it
against a different kernel out tree:

```sh
make -C "$KERNEL_OBJ" M="$PWD" LLVM=1 CONFIG_BASIC_MODVERSIONS=y modules
"$KERNEL_OBJ/scripts/sign-file" sha256 \
  "$KERNEL_OBJ/certs/signing_key.pem" \
  "$KERNEL_OBJ/certs/signing_key.x509" \
  "$PWD/llm_async_demo.ko"
```

Install the module in a private rootfs and load it at boot using
`/etc/modules-load.d/llm_async_demo.conf` containing `llm_async_demo`.
Load it only in the isolated experiment VM. `buggy_release=1` deliberately
emits `release` before `callback_exit` while still waiting before freeing the
object; this calibrates the oracle and is not a kernel vulnerability result.

The fixture waits for `callback_enter` before `submit` returns. This makes the
normal and `buggy_release=1` profiles deterministic: the normal path waits for
callback completion before release, while the calibration path emits release
before callback completion. The latter is test instrumentation and must never
be reported as a production kernel vulnerability.

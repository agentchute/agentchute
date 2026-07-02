# Disposable Python Conformance Proof

This is a point-in-time proof that the JSON vectors in `../vectors/` are
language-neutral. It is not a maintained Python SDK or a release gate.

Run manually:

```sh
python3 runner.py
```

The script uses Python stdlib only and implements the private-inbox binding
against the same vectors the Go conformance suite embeds.

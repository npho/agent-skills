# Optional skl profiles

Place TOML profile definitions in this directory. No profile is active by
default. A profile may select canonical `namespace/name` skills and extend other
profiles:

```toml
extends = ["base"]
skills = [
  "owner/skill-name",
]
```

Projects resolve profile contents only during `skl project init`, `project add`,
or the explicit `project refresh` operation.

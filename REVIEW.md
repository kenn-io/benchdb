# Review guidance

Review agents must never assess or predict whether code compiles. Compilation
belongs to the compiler and build/test jobs, not to code review.

Do not raise compilation, type-checking, import-resolution, undefined-symbol,
or language-version findings from source inspection. This prohibition applies
even when the code looks invalid or unfamiliar. Do not invent compiler
diagnostics or contradict successful compiler results with a source-level guess.

Actual compiler and CI output are authoritative for the targets they checked.
Report that output only as a check result, not as an agent-generated review
finding. If no compiler result is available, leave compilation unassessed.

Focus review findings on runtime behavior, logic, and API contracts that the
compiler cannot verify.

# scripts/air-gap

Helpers referenced from `docs/air-gap-install.md`. Each script is
standalone, uses only tools listed in its header, and exits non-zero
on any failure. No script reads or writes state outside its own
working directory except where explicitly noted.

| Script                 | Purpose                                                  |
| ---------------------- | -------------------------------------------------------- |
| `export-gguf.sh`       | Resolve an `ollama pull`-ed model tag to its GGUF blob.  |
| `oras-push-model.sh`   | Push a GGUF + Modelfile as an OCI artefact via ORAS.     |
| `oras-pull-model.sh`   | Pull an OCI artefact and reassemble it for `ollama create`. |
| `mirror-images.sh`     | `crane copy` a list of images into an internal registry. |

The scripts are deliberately small and `set -euo pipefail`-clean so
that a failure in any step surfaces immediately — "air-gap install
looked green but the model was silently missing" is the specific
outcome they exist to prevent.

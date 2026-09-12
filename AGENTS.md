# Models

Two models are available for this project.  Reasoning behind this separation of duties is to limit output tokens from the main model, and thus reduce cost. Each model is described below.

- glm-5.3 (provider: opencode) -- This model is for planning, design and thinking, not actually writing code.
- qwen3.6-moe:35b-a3b (provider: flm) -- This model turns the clear, detailed design from glm-5.3 into on-disk files. It also executes all tests. It can perform all file mutation operations. This model only has 32k of context, so keep assigned tasks small. Assigned tasks to this model should start with an empty context for best results. It exists as a local alternative to cloud-based models that have high turn counts and output tokens.

# Local Archive

`local/` stores local attachments unrelated to gateway operation or builds that must be kept; it is already ignored by `.gitignore`.

The current `local/agent-loop-plugin.js` is the original root-level `index.js`, an empty stub belonging to another host plugin, not this project's web entry; the file content is unchanged.

Do not store in-use configs or credentials here. Runtime logs still go to `logs/`, build artifacts to `dist/`, and public docs to `docs/`.

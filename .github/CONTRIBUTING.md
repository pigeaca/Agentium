# Contributing to Agentium

Agentium is at the research stage. The [feasibility study](../docs/research/2026-09-27-ai-development-lab.md) proposes the product, but no design is accepted yet, so there is no product code to change. The repository holds the development process that agents and people follow.

## Before you start

- For anything beyond a small fix, open an issue first so the scope is agreed before work starts. Use the **Proposal or feedback** form for product ideas and comments on the study, and the **Bug report** form for the harness, hooks, CI or agent instructions.
- Report security problems privately as described in the [security policy](SECURITY.md), not in an issue.
- Everyone who takes part follows the [code of conduct](CODE_OF_CONDUCT.md).

## How changes are made

The rules in [`.agents/`](../.agents/README.md) apply to people as well as agents. In short:

1. Write a plan sized to the change, with its outcome, scope, acceptance criteria and verification ([plan rules](../.agents/plans/README.md)).
2. Follow the [Git workflow](../.agents/rules/git-workflow.md) for branches and commit messages (`type(scope): outcome`).
3. Enable the pre-commit guard once per clone, and run the checks the diff needs before you push:

   ```sh
   python3 .agents/scripts/harness.py hooks
   python3 .agents/scripts/harness.py check changed
   ```

4. Open a pull request against `main` and fill in the template. CI must pass. The maintainer reviews and merges.

Never commit secrets, tokens or `.env` files ([secrets](../.agents/rules/secrets.md)). New dependencies need agreement first ([dependencies](../.agents/rules/supply-chain.md)).

## License

Contributions are licensed under the [Apache License 2.0](../LICENSE), the project's license.

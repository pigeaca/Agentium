# Secrets and private data

- Never write real credentials to source, docs, plans, screenshots, output or logs. Inspect configuration keys without printing values. Use ignored runtime environment/configuration and placeholder-only `.env.example`.
- Keep `.env*`, private keys/certificates, credential JSON, token files, local databases and dumps ignored. The pre-commit guard rejects credential-shaped additions and env/key files; it is a safety net, not a complete scanner, so still inspect the staged diff before committing.
- This repository is public. Anything committed or pushed is published, including plans, reports and screenshots.
- Never put secrets into URL query strings, container image layers, exception text or debug dumps. Keep CI credentials masked.
- External data transmission, credential storage and live integration setup require explicit scope. Ask for secure local configuration rather than secret values in chat.

# Errors

Wrap errors with the operation and relevant identifiers, and keep them matchable by type or sentinel. Honor cancellation and clean up resources on every path. Never swallow errors silently or replace an actionable message with a raw stack. User-facing errors name the failed action and a useful next step without exposing secrets.

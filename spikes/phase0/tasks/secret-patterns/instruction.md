Extend the staged credential scan with two more key shapes and report them with these exact labels:
- "Google API key": `AIza` followed by exactly 35 characters from `[0-9A-Za-z_-]`;
- "Stripe secret key": `sk_live_` followed by at least 24 letters or digits.

Test keys are not live secrets: `sk_test_...` must not be reported, and the existing `secret-scan: allow` opt-out must keep working.

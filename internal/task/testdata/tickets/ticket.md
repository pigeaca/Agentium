---
key: WEB-9
priority: high
---
# Show a friendly 404 page

## Description

Unknown URLs show the server's default page.<br>
Render <strong>templates/404.html</strong> instead &mdash; with the site header.

```html
<p>Keep this <b>as is</b></p>
```

## Acceptance criteria

- [ ] Unknown URLs answer 404 with the template
- [x] The header shows the
  current user
  - signed out: a sign-in link
1. Static files still 404 plainly

## Notes

Design: see the mockups.

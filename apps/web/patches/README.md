# Base UI focus guards

`@base-ui/react@1.9.0` deliberately gives its invisible focus guards a button
role on WebKit so VoiceOver focus traversal stays inside a popup. Those guards
have no accessible name in the pinned release. The two-line CommonJS/ESM patch
names that boundary without changing its focus behavior, tab order, visibility,
or the library's browser detection. It does not hide an actionable control from
assistive technology.

Bun applies the patch through `patchedDependencies`; the API Docker build copies
it before the frozen install. Keep it until the upstream guards provide an
accessible name. Verify it when updating Base UI with the open-menu light/dark
and touch cases in `scripts/qa/dashboard_density.py --engine webkit`. Automated
WebKit results do not replace a VoiceOver/Safari check.

# OpenGTM web app

React 19 + TypeScript + Vite + Tailwind 4 + shadcn/ui frontend for OpenGTM.

```bash
bun install
bun run dev        # stable https://opengtm.localhost via Portless
bun run dev:app    # plain Vite dev server on :4099
bun run build      # tsc -b && vite build → dist/
bun run lint
```

The app talks to the Go API. Vite proxies requests through nginx at
`http://127.0.0.1:3000` by default, matching Docker Compose. Set
`VITE_API_TARGET` to a bare Go API address (for example,
`http://127.0.0.1:8000`) to skip nginx. See the root
[README](../../README.md) and https://opengtm.palash.dev for the full setup.

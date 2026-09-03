# OpenGTM web app

React 19 + TypeScript + Vite + Tailwind 4 + shadcn/ui frontend for OpenGTM.

```bash
bun install
bun run dev        # stable https://opengtm.localhost via Portless
bun run dev:app    # plain Vite dev server on :5173
bun run build      # tsc -b && vite build → dist/
bun run lint
```

The app talks to the FastAPI backend (default `http://localhost:8000`, or the
nginx front on `:3000` in Docker Compose). See the root
[README](../../README.md) and https://opengtm.palash.dev for the full setup.

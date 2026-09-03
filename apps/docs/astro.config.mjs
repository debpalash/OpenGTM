// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import sitemap from '@astrojs/sitemap';
import starlightOpenAPI, { openAPISidebarGroups } from 'starlight-openapi';

const SITE = 'https://opengtm.palash.dev';
const REPO = 'https://github.com/debpalash/opengtm';

// https://astro.build/config
export default defineConfig({
  site: SITE,
  trailingSlash: 'ignore',
  integrations: [
    starlight({
      title: 'OpenGTM',
      description:
        'OpenGTM is a free, open-source, self-hostable alternative to Clay: source leads, run enrichment waterfalls, research them with AI, and push to your CRM — on your own infrastructure, with your own keys.',
      logo: {
        light: './src/assets/opengtm-lockup.svg',
        dark: './src/assets/opengtm-lockup-dark.svg',
        replacesTitle: true,
      },
      favicon: '/favicon.svg',
      social: [{ icon: 'github', label: 'GitHub', href: REPO }],
      editLink: { baseUrl: `${REPO}/edit/main/apps/docs/` },
      lastUpdated: true,
      customCss: ['./src/styles/custom.css'],
      head: [
        { tag: 'meta', attrs: { property: 'og:image', content: `${SITE}/og.png` } },
        { tag: 'meta', attrs: { name: 'twitter:card', content: 'summary_large_image' } },
      ],
      plugins: [
        starlightOpenAPI([
          {
            base: 'api',
            label: 'REST API',
            schema: './openapi/openapi.json',
            sidebar: { collapsed: true, operations: { badges: true } },
          },
        ]),
      ],
      sidebar: [
        {
          label: 'Getting started',
          items: [
            { label: 'Introduction', slug: 'getting-started/introduction' },
            { label: 'Quickstart (Docker)', slug: 'getting-started/quickstart' },
            { label: 'Local development', slug: 'getting-started/local-development' },
            { label: 'Your first workbook', slug: 'getting-started/first-workbook' },
          ],
        },
        {
          label: 'Concepts',
          items: [{ autogenerate: { directory: 'concepts' } }],
        },
        {
          label: 'Guides',
          items: [{ autogenerate: { directory: 'guides' } }],
        },
        {
          label: 'Integrations',
          items: [{ autogenerate: { directory: 'integrations' } }],
        },
        {
          label: 'Self-hosting',
          items: [{ autogenerate: { directory: 'self-hosting' } }],
        },
        {
          label: 'Reference',
          items: [{ autogenerate: { directory: 'reference' } }],
        },
        ...openAPISidebarGroups,
        {
          label: 'Community',
          items: [{ autogenerate: { directory: 'community' } }],
        },
      ],
    }),
    sitemap(),
  ],
});

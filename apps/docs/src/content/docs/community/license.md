---
title: License
description: OpenGTM is AGPLv3. What that means for self-hosting and for offering it as a service.
sidebar:
  order: 3
---

OpenGTM is licensed under the
[GNU Affero General Public License v3.0](https://github.com/debpalash/opengtm/blob/main/LICENSE).

In plain terms:

- **Self-host it for your own team, free, with no strings on internal use.**
  Use it, modify it, run it on your own servers.
- **The AGPL network clause applies only if you offer OpenGTM (or a modified
  version) as a service to other people over a network.** In that case you must
  make your modified source available to those users. Running it internally does
  not trigger that obligation.

This is not legal advice; read the full licence if you plan to offer OpenGTM as
a hosted service.

## Commercial licence

If the AGPL does not fit your use, for example a proprietary deployment or
product, Yupcha Softwares Private Limited offers OpenGTM under a commercial
licence. Contact legal@yupcha.com.

## Contributions

Contributors sign the
[Contributor License Agreement](https://github.com/debpalash/opengtm/blob/main/CLA.md).
Contributors keep their copyright, and grant Yupcha the right to distribute
their work under the AGPL and under commercial licences. Whatever Yupcha
includes in the public source release stays available under the AGPL or
another OSI-approved licence.

## Third-party components

- The optional [Reacher](https://github.com/reacherhq/check-if-email-exists)
  email-verification service is dual-licensed AGPL-3.0 / commercial. OpenGTM
  runs the AGPL build as a separate networked process; it is off unless
  `REACHER_ENABLED=1`.
- The n8n community node under `packages/n8n-nodes-yupcha` is MIT licensed so it
  can be published to the n8n registry.
- Python and JavaScript dependencies carry their own licences; see `uv.lock`
  and `bun.lock` for the exact versions shipped.

# Source brand artwork

All images are bundled locally; the dashboard never requests a logo from a
third-party service. `manifest.json` records the original brand source, asset
distribution URL and any format exception for every source ID.

SVG marks come from the companies' published assets or established brand-mark
distributions (Simple Icons and SVGL), with the original source retained in the
manifest. Simple Icons glyphs use their declared icon license, normally CC0-1.0;
individual exceptions and brand guidelines follow the referenced source.

Some brands publish only raster favicons. Those original PNG images are embedded
unchanged in an SVG container for consistent layout. Their manifest `format`
identifies the raster content; the container does not make it a vector drawing.
Where the origin does not expose a downloadable icon, a cached site favicon is
bundled and the cache URL is recorded. No runtime logo API or tracking is used.

Names and marks belong to their respective owners. Their use identifies the
data sources; it does not imply endorsement. Spicrawl is the separately
identified strategic sponsor.

Generic categories such as Company Lists and Industry Associations have no
single brand owner and retain their category icon.

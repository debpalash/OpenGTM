// OpenGTM's quiet surfaces do not use Twenty's embedded decorative PNG.
// Replace only that token; preserve the upstream package and all semantic tokens.
export function twentyCss() {
  return {
    postcssPlugin: "opengtm-twenty-css",
    Declaration(declaration: { prop: string; value: string }) {
      if (declaration.prop === "--t-background-noisy") declaration.value = "none"
    },
  }
}

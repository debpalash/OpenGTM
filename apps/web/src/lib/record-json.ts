// External IDs often exceed JavaScript's safe integer range. Keep their exact
// digits for display/editing; untouched fields are omitted from PATCH requests.
// Raw import bodies and download responses preserve their original JSON types.
export function parseRecordJSON(text: string): unknown {
  return JSON.parse(text, ((
    _: string,
    value: unknown,
    context?: { source?: string }
  ) => {
    if (
      typeof value === "number" &&
      Number.isInteger(value) &&
      !Number.isSafeInteger(value)
    ) {
      if (context?.source && /^-?\d+$/.test(context.source))
        return context.source
      throw new Error(
        "This browser cannot safely display a large numeric field. Download the original JSON or use an updated browser."
      )
    }
    return value
  }) as Parameters<typeof JSON.parse>[1])
}

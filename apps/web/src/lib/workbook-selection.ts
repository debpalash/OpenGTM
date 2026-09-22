/** Never reinterpret a lead ID as a workbook row ID or delete a global lead. */
export function selectedWorkbookRowIds(selection: Record<string, boolean>): number[] {
  return Object.entries(selection).filter(([, selected]) => selected).map(([key]) => {
    if (!/^row:[1-9]\d*$/.test(key) || !Number.isSafeInteger(Number(key.slice(4)))) {
      throw new Error("This selection includes legacy lead rows. Migrate this workbook before deleting or exporting selected rows. Original leads have not been changed.")
    }
    return Number(key.slice(4))
  })
}

export type WorkbookDeleteScope =
  | { kind: "rows"; rowIds: number[] }
  | { kind: "matching"; count: number; viewId: string | null; search: string }

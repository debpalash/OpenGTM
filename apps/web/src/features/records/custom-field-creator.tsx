import { useState } from "react"
import { Button, Field, Input, NativeSelect } from "@/design-system/primitives"
import type { FieldType } from "./record-draft"

export function CustomFieldCreator({
  disabled,
  pending,
  onPendingChange,
  onAdd
}: {
  disabled: boolean
  pending: { name: string; type: FieldType }
  onPendingChange: (pending: { name: string; type: FieldType }) => void
  onAdd: (name: string, type: FieldType) => void
}) {
  const { name, type } = pending
  const [error, setError] = useState("")
  function add() {
    try {
      onAdd(name, type)
      setError("")
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not add field.")
    }
  }
  return (
    <fieldset className="record-custom-field" disabled={disabled}>
      <legend>Add a custom field</legend>
      <Field
        id="record-custom-name"
        label="Custom field name"
        error={error || undefined}
      >
        {(control) => (
          <Input
            {...control}
            value={name}
            onChange={(event) => {
              onPendingChange({ ...pending, name: event.target.value })
              setError("")
            }}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault()
                add()
              }
            }}
          />
        )}
      </Field>
      <Field label="Custom field type">
        {(control) => (
          <NativeSelect
            {...control}
            value={type}
            onChange={(event) =>
              onPendingChange({
                ...pending,
                type: event.target.value as FieldType
              })
            }
          >
            <option value="text">Text</option>
            <option value="number">Number</option>
            <option value="boolean">Yes / No</option>
            <option value="json">JSON</option>
          </NativeSelect>
        )}
      </Field>
      <Button
        type="button"
        variant="outline"
        disabled={!name.trim() || disabled}
        onClick={add}
      >
        Add custom field
      </Button>
    </fieldset>
  )
}

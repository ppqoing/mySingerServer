import type { ReactNode } from 'react'
import { FormField } from './FormField'

type PathPickerProps = {
  id: string
  name: string
  label: string
  value: string
  error?: string
  help?: ReactNode
  disabled?: boolean
  choosePath?: (currentPath: string) => Promise<string>
  onChange: (value: string) => void
  onBlur?: () => void
}

export function PathPicker({
  id,
  name,
  label,
  value,
  error,
  help,
  disabled = false,
  choosePath,
  onChange,
  onBlur,
}: PathPickerProps): ReactNode {
  const select = async (): Promise<void> => {
    if (!choosePath) {
      return
    }
    const selected = (await choosePath(value)).trim()
    if (selected && isLocalAbsolutePath(selected)) {
      onChange(selected)
    }
  }

  return (
    <div className="path-picker">
      <FormField id={id} label={label} help={help} error={error}>
        <input
          name={name}
          type="text"
          value={value}
          disabled={disabled}
          onChange={(event) => onChange(event.currentTarget.value)}
          onBlur={onBlur}
        />
      </FormField>
      <button
        className="button-secondary"
        type="button"
        disabled={disabled || !choosePath}
        aria-label={`选择${label}`}
        onClick={() => void select()}
      >
        选择…
      </button>
    </div>
  )
}

function isLocalAbsolutePath(value: string): boolean {
  return /^[A-Za-z]:[\\/](?:[^\\/]+(?:[\\/]|$))*$/.test(value)
    || /^\\\\[^\\/]+\\[^\\/]+(?:\\[^\\/]+)*\\?$/.test(value)
}

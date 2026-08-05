import { cloneElement, type ReactElement, type ReactNode } from 'react'

type FieldControlProps = {
  id?: string
  'aria-describedby'?: string
  'aria-invalid'?: boolean
  'aria-errormessage'?: string
}

type FormFieldProps = {
  id: string
  label: string
  help?: ReactNode
  error?: ReactNode
  children: ReactElement<FieldControlProps>
}

export function FormField({ id, label, help, error, children }: FormFieldProps): ReactNode {
  const helpID = help ? `${id}-help` : undefined
  const errorID = error ? `${id}-error` : undefined
  const describedBy = [helpID, errorID].filter(Boolean).join(' ') || undefined
  const control = cloneElement(children, {
    id,
    'aria-describedby': describedBy,
    'aria-invalid': error ? true : undefined,
    'aria-errormessage': errorID,
  })

  return (
    <div className="form-field">
      <label className="form-field__label" htmlFor={id}>
        {label}
      </label>
      {control}
      {help ? (
        <p className="form-field__help" id={helpID}>
          {help}
        </p>
      ) : null}
      {error ? (
        <p className="form-field__error" id={errorID} role="alert">
          {error}
        </p>
      ) : null}
    </div>
  )
}

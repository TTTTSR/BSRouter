import { useState } from 'react'
import type { ReactNode } from 'react'
import { IconX } from '../lib/icons'

type BtnVariant = 'primary' | 'secondary' | 'ghost'

export function Button({
  children, variant = 'secondary', onClick, disabled, type, title, className,
}: {
  children: ReactNode
  variant?: BtnVariant
  onClick?: () => void
  disabled?: boolean
  type?: 'button' | 'submit'
  title?: string
  className?: string
}) {
  return (
    <button
      type={type ?? 'button'}
      className={`btn ${variant}${className ? ' ' + className : ''}`}
      onClick={onClick}
      disabled={disabled}
      title={title}
    >
      {children}
    </button>
  )
}

export function Input(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={`input${props.className ? ' ' + props.className : ''}`} />
}

export function Select(props: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...props} className={`select${props.className ? ' ' + props.className : ''}`} />
}

export function TextArea(props: React.TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...props} className={`textarea${props.className ? ' ' + props.className : ''}`} />
}

export function Field({
  label, hint, className, children,
}: {
  label: string
  hint?: string
  className?: string
  children: ReactNode
}) {
  return (
    <label className={`field${className ? ' ' + className : ''}`}>
      <span className="field-label">{label}</span>
      {children}
      {hint ? <span className="form-hint">{hint}</span> : null}
    </label>
  )
}

export function Badge({ children, invert }: { children: ReactNode; invert?: boolean }) {
  return <span className={`badge${invert ? ' invert' : ''}`}>{children}</span>
}

export function Spinner() {
  return <span className="spinner" role="status" aria-label="loading" />
}

export function Empty({ text }: { text: string }) {
  return <div className="empty">{text}</div>
}

export function ErrorAlert({ text }: { text: string }) {
  return <div className="alert error-text">{text}</div>
}

export function Modal({
  title, onClose, children, footer, wide,
}: {
  title: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  wide?: boolean
}) {
  return (
    <div className="modal-overlay" onClick={onClose}>
      <div
        className="modal"
        style={wide ? { maxWidth: '720px' } : undefined}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-head">
          <span className="modal-title">{title}</span>
          <Button variant="ghost" className="icon-btn" onClick={onClose} title="关闭">
            <IconX />
          </Button>
        </div>
        <div className="modal-body">{children}</div>
        {footer ? <div className="modal-foot">{footer}</div> : null}
      </div>
    </div>
  )
}

// TagList 标签列表视图:每个值一个标签,可单独删除;输入框回车或失去焦点时添加。
export function TagList({
  values, onChange, placeholder = '输入后回车添加',
}: {
  values: string[]
  onChange: (v: string[]) => void
  placeholder?: string
}) {
  const [draft, setDraft] = useState('')

  function add() {
    const v = draft.trim()
    if (v === '' || values.includes(v)) return
    onChange([...values, v])
    setDraft('')
  }

  return (
    <div className="taglist">
      <div className="taglist-tags">
        {values.length === 0 ? (
          <span className="faint" style={{ fontSize: 12 }}>暂无</span>
        ) : (
          values.map((v) => (
            <span key={v} className="tag">
              <span className="tag-label">{v}</span>
              <button
                type="button"
                className="tag-x"
                title="移除"
                aria-label={`移除 ${v}`}
                onClick={() => onChange(values.filter((x) => x !== v))}
              >
                <IconX size={11} />
              </button>
            </span>
          ))
        )}
      </div>
      <input
        className="input"
        value={draft}
        placeholder={placeholder}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            add()
          }
        }}
      />
    </div>
  )
}

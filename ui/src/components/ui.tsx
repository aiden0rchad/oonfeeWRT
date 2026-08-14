import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'

/** Status pill. The dot is never the only signal — UI-SPEC §3 calls colour-only
 *  status a genuine accessibility flaw and says not to inherit it, so the text
 *  always ships alongside. */
export function Status({ value }: { value: string }) {
  const colour =
    value === 'online' || value === 'wireless'
      ? 'var(--good)'
      : value === 'offline' || value === 'blocked'
        ? 'var(--critical)'
        : value === 'pending'
          ? 'var(--warning)'
          : 'var(--text-muted)'
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      <span
        aria-hidden
        style={{
          width: 7,
          height: 7,
          borderRadius: '50%',
          background: colour,
          flex: '0 0 auto',
        }}
      />
      <span>{value}</span>
    </span>
  )
}

export function Card({
  title,
  actions,
  children,
  pad = true,
}: {
  title?: ReactNode
  actions?: ReactNode
  children: ReactNode
  pad?: boolean
}) {
  return (
    <section
      style={{
        background: 'var(--surface-1)',
        border: '1px solid var(--border)',
        borderRadius: 8,
        overflow: 'hidden',
      }}
    >
      {title && (
        <header
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: 12,
            padding: '10px 14px',
            borderBottom: '1px solid var(--border)',
            fontSize: 13,
            fontWeight: 600,
          }}
        >
          <span>{title}</span>
          {actions}
        </header>
      )}
      <div style={{ padding: pad ? 14 : 0 }}>{children}</div>
    </section>
  )
}

/** A hero number with its label. tabular-nums so a changing value does not
 *  make the layout jitter. */
export function Stat({
  label,
  value,
  tone,
}: {
  label: string
  value: ReactNode
  tone?: 'good' | 'warning' | 'critical' | 'muted'
}) {
  const colour = tone ? `var(--${tone === 'muted' ? 'text-muted' : tone})` : undefined
  return (
    <div>
      <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>{label}</div>
      <div
        className="num"
        style={{
          fontSize: 30,
          fontWeight: 600,
          textAlign: 'left',
          color: colour,
          lineHeight: 1.15,
        }}
      >
        {value}
      </div>
    </div>
  )
}

export function Button({
  children,
  onClick,
  kind = 'default',
  disabled,
  type = 'button',
}: {
  children: ReactNode
  onClick?: () => void
  kind?: 'default' | 'primary'
  disabled?: boolean
  type?: 'button' | 'submit'
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      style={{
        height: 28,
        padding: '0 12px',
        borderRadius: 6,
        fontSize: 12,
        fontWeight: 500,
        cursor: disabled ? 'default' : 'pointer',
        opacity: disabled ? 0.55 : 1,
        color: kind === 'primary' ? '#fff' : 'var(--text-primary)',
        background: kind === 'primary' ? 'var(--accent)' : 'var(--surface-2)',
        border: `1px solid ${kind === 'primary' ? 'var(--accent)' : 'var(--border-strong)'}`,
      }}
    >
      {children}
    </button>
  )
}

// React 19 passes `ref` through as an ordinary prop on function components, so
// the spread below forwards it without forwardRef. The type has to say so.
export function Field({
  label,
  ...props
}: { label: string } & React.ComponentPropsWithRef<'input'>) {
  return (
    <label style={{ display: 'block' }}>
      <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>
        {label}
      </div>
      <input
        {...props}
        style={{
          width: '100%',
          height: 30,
          padding: '0 10px',
          borderRadius: 6,
          background: 'var(--surface-0)',
          border: '1px solid var(--border-strong)',
          color: 'var(--text-primary)',
          fontSize: 13,
        }}
      />
    </label>
  )
}

/** Shown wherever a value is genuinely unknown, so an empty cell can never be
 *  mistaken for a zero. The title carries the reason. */
export function Unknown({ why }: { why: string }) {
  return (
    <span title={why} style={{ color: 'var(--text-muted)' }}>
      —
    </span>
  )
}

export function Banner({
  tone = 'warning',
  children,
}: {
  tone?: 'warning' | 'critical' | 'accent'
  children: ReactNode
}) {
  const colour = tone === 'accent' ? 'var(--accent)' : `var(--${tone})`
  return (
    <div
      style={{
        border: `1px solid ${colour}`,
        borderLeftWidth: 3,
        borderRadius: 6,
        padding: '8px 12px',
        fontSize: 12,
        color: 'var(--text-primary)',
        background: 'var(--surface-1)',
      }}
    >
      {children}
    </div>
  )
}

/** Column definition for DataGrid. */
export interface Column<T> {
  key: string
  header: string
  /** Right-aligns and applies tabular figures. */
  numeric?: boolean
  width?: number
  render: (row: T) => ReactNode
  /** Sort value; omit to make the column unsortable. */
  sortBy?: (row: T) => string | number
}

/**
 * The one grid, per UI-SPEC §5: sticky header, semantic alignment, click a row
 * to open its detail.
 *
 * Sorting and filtering happen here over the whole result set rather than over
 * a visible page, because a filter count computed from the loaded page is a
 * lie — the spec calls that out specifically.
 */
export function DataGrid<T>({
  rows,
  columns,
  rowKey,
  onRowClick,
  empty = 'Nothing to show',
}: {
  rows: T[]
  columns: Column<T>[]
  rowKey: (row: T) => string
  onRowClick?: (row: T) => void
  empty?: ReactNode
}) {
  const [sort, setSort] = useState<{ key: string; dir: 1 | -1 } | null>(null)

  let view = rows
  if (sort) {
    const col = columns.find((c) => c.key === sort.key)
    if (col?.sortBy) {
      view = [...rows].sort((a, b) => {
        const av = col.sortBy!(a)
        const bv = col.sortBy!(b)
        if (av === bv) return 0
        return (av < bv ? -1 : 1) * sort.dir
      })
    }
  }

  if (rows.length === 0) {
    return (
      <div style={{ padding: 24, color: 'var(--text-secondary)', fontSize: 12 }}>
        {empty}
      </div>
    )
  }

  return (
    <div style={{ overflowX: 'auto' }}>
      <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
        <thead>
          <tr>
            {columns.map((c) => (
              <th
                key={c.key}
                onClick={() =>
                  c.sortBy &&
                  setSort((s) =>
                    s?.key === c.key
                      ? { key: c.key, dir: s.dir === 1 ? -1 : 1 }
                      : { key: c.key, dir: 1 },
                  )
                }
                style={{
                  position: 'sticky',
                  top: 0,
                  zIndex: 1,
                  background: 'var(--surface-1)',
                  borderBottom: '1px solid var(--border)',
                  padding: '8px 12px',
                  textAlign: c.numeric ? 'right' : 'left',
                  fontSize: 11,
                  fontWeight: 600,
                  color: 'var(--text-secondary)',
                  whiteSpace: 'nowrap',
                  cursor: c.sortBy ? 'pointer' : 'default',
                  width: c.width,
                  userSelect: 'none',
                }}
              >
                {c.header}
                {sort?.key === c.key && (sort.dir === 1 ? ' ↑' : ' ↓')}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {view.map((row) => (
            <tr
              key={rowKey(row)}
              onClick={() => onRowClick?.(row)}
              style={{
                cursor: onRowClick ? 'pointer' : 'default',
                borderBottom: '1px solid var(--border)',
              }}
              onMouseEnter={(e) =>
                (e.currentTarget.style.background = 'var(--surface-2)')
              }
              onMouseLeave={(e) => (e.currentTarget.style.background = '')}
            >
              {columns.map((c) => (
                <td
                  key={c.key}
                  className={c.numeric ? 'num' : undefined}
                  style={{ padding: '7px 12px', height: 33, whiteSpace: 'nowrap' }}
                >
                  {c.render(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** Slide-over detail panel — 370px, enters from the right (UI-SPEC §1). */
export function SlideOver({
  title,
  onClose,
  children,
}: {
  title: ReactNode
  onClose: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    ref.current?.focus()
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div
      ref={ref}
      tabIndex={-1}
      role="dialog"
      aria-label={typeof title === 'string' ? title : 'Detail'}
      style={{
        position: 'fixed',
        top: 40,
        right: 0,
        bottom: 0,
        width: 370,
        background: 'var(--surface-1)',
        borderLeft: '1px solid var(--border)',
        overflowY: 'auto',
        zIndex: 20,
      }}
    >
      <header
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          padding: '12px 14px',
          borderBottom: '1px solid var(--border)',
          position: 'sticky',
          top: 0,
          background: 'var(--surface-1)',
        }}
      >
        <strong style={{ fontSize: 13 }}>{title}</strong>
        <button
          onClick={onClose}
          aria-label="Close"
          style={{
            background: 'none',
            border: 'none',
            color: 'var(--text-secondary)',
            cursor: 'pointer',
            fontSize: 18,
            lineHeight: 1,
          }}
        >
          ×
        </button>
      </header>
      <div style={{ padding: 14, display: 'grid', gap: 14 }}>{children}</div>
    </div>
  )
}

/** A label/value row inside a slide-over. */
export function Prop({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12 }}>
      <span style={{ fontSize: 12, color: 'var(--text-secondary)' }}>{label}</span>
      <span style={{ fontSize: 12, fontWeight: 500, textAlign: 'right' }}>
        {children}
      </span>
    </div>
  )
}

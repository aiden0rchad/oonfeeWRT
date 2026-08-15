import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { moveColumn, orderColumns, parsePrefs } from '../lib/columns'
import type { ColumnPrefs } from '../lib/columns'

export type { ColumnPrefs }
export { orderColumns, moveColumn }

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
  sub,
}: {
  label: string
  value: ReactNode
  tone?: 'good' | 'warning' | 'critical' | 'muted'
  /** A small line under the number, for what the number deliberately leaves
   *  out. A count that excludes something should say what, next to itself —
   *  an explanation two cards away does not get read. */
  sub?: ReactNode
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
      {sub != null && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 3 }}>
          {sub}
        </div>
      )}
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
  /** Cannot be hidden. For the column that identifies the row — hiding it
   *  leaves a grid of attributes belonging to nothing. */
  required?: boolean
}

/**
 * Nominal row height in px, and the line-height that makes it come out exactly.
 *
 * Virtualization needs to know where row N starts without having measured rows
 * 0..N-1, so the height has to be a constant. `height: 33` on a `<td>` does not
 * produce one: in a table that is a *minimum*, and the row is as tall as its
 * content — measured 33.84px here, from 14px of padding plus whatever the
 * font's default line box came to. Over 1000 rows that 0.84px compounds to
 * 840px, so the window drifts most of a screen out of position by the bottom.
 *
 * Pinning the line box makes the arithmetic exact (19 + 7 + 7 = 33), and the
 * grid measures a real row anyway rather than trusting this — a font that
 * renders differently would otherwise reintroduce the same drift silently.
 */
export const ROW_HEIGHT = 33
const ROW_LINE_HEIGHT = 19

/** Rows drawn above and below the viewport, so a fast scroll does not show
 *  blank space before React catches up. */
const OVERSCAN = 8

/** Beyond this many rows, switch to windowed rendering. Below it, the DOM cost
 *  is irrelevant and a plain table keeps ctrl-F working over the whole grid. */
const VIRTUALIZE_ABOVE = 150

/**
 * Height of the grid's scroll viewport.
 *
 * The grid scrolls itself rather than letting the page scroll it, and that is
 * what makes the sticky header actually stick. `position: sticky` resolves
 * against the nearest scrolling ancestor, and Card sets `overflow: hidden` for
 * its rounded corners — which makes Card that ancestor. The header was
 * therefore pinned to the top of a box that does not scroll, so it slid away
 * with the rows and looked exactly like a header that was never sticky at all.
 * Invisible until a grid had enough rows to scroll, which is why 13 clients
 * never showed it.
 *
 * Viewport-relative so a tall window gets a tall grid, capped so an enormous
 * one does not put the pager off-screen.
 */
const VIEWPORT_HEIGHT = 'min(70vh, 760px)'

/**
 * The one grid, per UI-SPEC §5: sticky header, semantic alignment, click a row
 * to open its detail, show/hide columns, and virtualized rows.
 *
 * Virtualization kicks in above VIRTUALIZE_ABOVE rows rather than always. Below
 * that the DOM cost does not matter, and a plain table keeps the browser's own
 * find-in-page working across every row — which windowing silently breaks,
 * because the rows that are not rendered cannot be found. Trading that away at
 * 13 rows would be a bad deal; at 10,000 the grid is unusable without it.
 */
export function DataGrid<T>({
  rows,
  columns,
  rowKey,
  onRowClick,
  empty = 'Nothing to show',
  prefs,
  onPrefsChange,
}: {
  rows: T[]
  columns: Column<T>[]
  rowKey: (row: T) => string
  onRowClick?: (row: T) => void
  empty?: ReactNode
  /** Column visibility and order. Omit to disable column customization. */
  prefs?: ColumnPrefs
  onPrefsChange?: (v: ColumnPrefs) => void
}) {
  const [sort, setSort] = useState<{ key: string; dir: 1 | -1 } | null>(null)
  const dragging = useRef<string | null>(null)
  // A finished drag must not also sort the column it landed on. Browsers differ
  // on whether a click follows a drag, so this is cheap insurance rather than a
  // fix for an observed bug — and getting it wrong means every reorder silently
  // re-sorts the grid.
  const swallowClick = useRef(false)
  const [scrollTop, setScrollTop] = useState(0)
  // Measured, not assumed: the CSS height is viewport-relative, and windowing
  // against a guessed height leaves blank rows on a tall screen.
  const [viewport, setViewport] = useState(600)
  // Also measured. See ROW_HEIGHT — a row that renders 0.84px taller than the
  // constant puts the window 840px out by row 1000.
  const [rowH, setRowH] = useState(ROW_HEIGHT)
  const scroller = useRef<HTMLDivElement>(null)
  const body = useRef<HTMLTableSectionElement>(null)

  const hiddenSet = new Set(prefs?.hidden ?? [])
  const ordered = orderColumns(columns, prefs?.order ?? [])
  const shown = ordered.filter((c) => c.required || !hiddenSet.has(c.key))

  // Reordering always rewrites the FULL key list, hidden columns included, so
  // a column unhidden later comes back where the operator left it.
  const reorder = (from: string, to: string) => {
    if (!prefs || !onPrefsChange) return
    onPrefsChange({
      ...prefs,
      order: moveColumn(ordered.map((c) => c.key), from, to),
    })
  }

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

  const virtual = view.length > VIRTUALIZE_ABOVE
  // Track the real viewport height so the window matches what is on screen
  // rather than a constant that happens to be wrong on a short window.
  useEffect(() => {
    if (!scroller.current) return
    const el = scroller.current
    const ro = new ResizeObserver(() => setViewport(el.clientHeight))
    ro.observe(el)
    setViewport(el.clientHeight)
    return () => ro.disconnect()
  }, [])

  // Measure a real row. One row is enough — they are uniform by construction —
  // and re-measuring on every render would fight with the window it feeds.
  useEffect(() => {
    const tr = body.current?.querySelector('tr[data-row]')
    if (!tr) return
    const h = tr.getBoundingClientRect().height
    if (h > 0 && Math.abs(h - rowH) > 0.5) setRowH(h)
  }, [rowH, columns.length, prefs?.hidden])

  // Keep React's idea of the scroll position tied to the DOM's.
  //
  // They diverged the moment virtualization engaged: the handler was only
  // attached while `virtual` was true, so scrolling a short grid updated
  // nothing, and growing it past the threshold rendered a window for
  // scrollTop 0 while the element sat at 1000px — a header above a completely
  // blank grid. The handler is now unconditional, and this re-reads the
  // element whenever the row set changes underneath it.
  useEffect(() => {
    const el = scroller.current
    if (el && el.scrollTop !== scrollTop) setScrollTop(el.scrollTop)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view.length, virtual])

  let first = 0
  let last = view.length
  if (virtual) {
    first = Math.max(0, Math.floor(scrollTop / rowH) - OVERSCAN)
    last = Math.min(view.length, Math.ceil((scrollTop + viewport) / rowH) + OVERSCAN)
  }
  const slice = virtual ? view.slice(first, last) : view

  if (rows.length === 0) {
    return (
      <div style={{ padding: 24, color: 'var(--text-secondary)', fontSize: 12 }}>
        {empty}
      </div>
    )
  }

  const header = (
    <thead>
      <tr>
        {shown.map((c) => (
          <th
            key={c.key}
            draggable={!!onPrefsChange}
            onDragStart={(e) => {
              dragging.current = c.key
              e.dataTransfer.effectAllowed = 'move'
              // Firefox will not start a drag without payload.
              e.dataTransfer.setData('text/plain', c.key)
            }}
            onDragOver={(e) => {
              if (dragging.current && dragging.current !== c.key) e.preventDefault()
            }}
            onDrop={(e) => {
              e.preventDefault()
              const from = dragging.current
              dragging.current = null
              swallowClick.current = true
              if (from) reorder(from, c.key)
            }}
            onDragEnd={() => {
              dragging.current = null
            }}
            onClick={() => {
              if (swallowClick.current) {
                swallowClick.current = false
                return
              }
              if (!c.sortBy) return
              setSort((s) =>
                s?.key === c.key
                  ? { key: c.key, dir: s.dir === 1 ? -1 : 1 }
                  : { key: c.key, dir: 1 },
              )
            }}
            title={onPrefsChange ? 'Drag to reorder' : undefined}
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
  )

  const bodyEl = (
    <tbody ref={body}>
      {/* Spacers carry the height of the rows that are not rendered, so the
          scrollbar reflects the whole grid rather than the window. Without
          them the bar would jump as the window moved. */}
      {virtual && first > 0 && (
        <tr style={{ height: first * rowH }} aria-hidden>
          <td colSpan={shown.length} style={{ padding: 0, border: 'none' }} />
        </tr>
      )}
      {slice.map((row) => (
        <tr
          key={rowKey(row)}
          data-row
          onClick={() => onRowClick?.(row)}
          style={{
            cursor: onRowClick ? 'pointer' : 'default',
            borderBottom: '1px solid var(--border)',
          }}
          onMouseEnter={(e) => (e.currentTarget.style.background = 'var(--surface-2)')}
          onMouseLeave={(e) => (e.currentTarget.style.background = '')}
        >
          {shown.map((c) => {
            const cell = c.render(row)
            return (
              <td
                key={c.key}
                className={c.numeric ? 'num' : undefined}
                // A clipped cell has to be recoverable. Only plain text can go
                // in a title attribute, which is most of what gets clipped.
                title={typeof cell === 'string' ? cell : undefined}
                style={{
                  padding: '7px 12px',
                  height: ROW_HEIGHT,
                  lineHeight: `${ROW_LINE_HEIGHT}px`,
                  whiteSpace: 'nowrap',
                  // Fixed layout does not grow a column to fit its content, so
                  // without this a long value runs straight over its neighbour
                  // — two columns of text on top of each other, both
                  // unreadable. Clip instead.
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                }}
              >
                {cell}
              </td>
            )
          })}
        </tr>
      ))}
      {virtual && last < view.length && (
        <tr style={{ height: (view.length - last) * rowH }} aria-hidden>
          <td colSpan={shown.length} style={{ padding: 0, border: 'none' }} />
        </tr>
      )}
    </tbody>
  )

  const table = (
    <table
      style={{
        width: '100%',
        borderCollapse: 'collapse',
        fontSize: 13,
        tableLayout: virtual ? 'fixed' : 'auto',
      }}
    >
      {header}
      {bodyEl}
    </table>
  )

  return (
    <div>
      {prefs !== undefined && onPrefsChange && (
        <ColumnPicker
          columns={ordered}
          hidden={hiddenSet}
          onChange={(keys) => onPrefsChange({ ...prefs, hidden: keys })}
          onMove={(key, delta) => {
            const keys = ordered.map((c) => c.key)
            const at = keys.indexOf(key)
            const to = keys[at + delta]
            if (to) reorder(key, to)
          }}
          virtualized={virtual}
          rowCount={view.length}
        />
      )}
      {/* Always its own scroll container, virtualized or not — see
          VIEWPORT_HEIGHT. A short grid never reaches the cap and so never
          shows an inner scrollbar. */}
      <div
        ref={scroller}
        onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
        style={{ overflow: 'auto', maxHeight: VIEWPORT_HEIGHT }}
      >
        {table}
      </div>
    </div>
  )
}

/**
 * Show, hide and reorder columns.
 *
 * The arrows are not a lesser alternative to dragging the headers — they are
 * the only path that works without a mouse, and the only one that can move a
 * HIDDEN column, which dragging cannot because there is no header to grab.
 * Someone who hides a column, rearranges the rest, then unhides it would
 * otherwise have no way to say where it goes.
 *
 * It also states when the grid is windowed, because that changes what the page
 * can do: the browser's find-in-page only sees rendered rows, so a search that
 * comes up empty on a virtualized grid is not evidence the value is absent.
 * Leaving that unsaid would turn a rendering optimisation into a silently wrong
 * answer.
 */
function ColumnPicker<T>({
  columns,
  hidden,
  onChange,
  onMove,
  virtualized,
  rowCount,
}: {
  columns: Column<T>[]
  hidden: Set<string>
  onChange: (keys: string[]) => void
  onMove: (key: string, delta: -1 | 1) => void
  virtualized: boolean
  rowCount: number
}) {
  const [open, setOpen] = useState(false)
  const nHidden = columns.filter((c) => !c.required && hidden.has(c.key)).length

  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        padding: '6px 12px',
        borderBottom: '1px solid var(--border)',
        fontSize: 11,
        color: 'var(--text-muted)',
      }}
    >
      <button
        onClick={() => setOpen((v) => !v)}
        style={{
          border: '1px solid var(--border-strong)',
          background: 'var(--surface-2)',
          color: 'var(--text-primary)',
          borderRadius: 4,
          padding: '2px 8px',
          fontSize: 11,
          cursor: 'pointer',
        }}
      >
        Customize columns{nHidden > 0 ? ` (${nHidden} hidden)` : ''}
      </button>
      {virtualized && (
        <span title="Find-in-page only searches rendered rows.">
          {rowCount.toLocaleString()} rows, drawn as you scroll — ⌘F searches
          only what is on screen
        </span>
      )}
      {open && (
        <div
          style={{
            display: 'flex',
            flexWrap: 'wrap',
            gap: 10,
            marginLeft: 4,
          }}
        >
          {columns.map((c, i) => (
            <span
              key={c.key}
              style={{ display: 'inline-flex', alignItems: 'center', gap: 2 }}
            >
              <label
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 4,
                  cursor: c.required ? 'default' : 'pointer',
                  opacity: c.required ? 0.5 : 1,
                  color: 'var(--text-secondary)',
                }}
                title={c.required ? 'This column identifies the row.' : undefined}
              >
                <input
                  type="checkbox"
                  disabled={c.required}
                  checked={c.required || !hidden.has(c.key)}
                  onChange={() => {
                    const next = new Set(hidden)
                    if (next.has(c.key)) next.delete(c.key)
                    else next.add(c.key)
                    onChange([...next])
                  }}
                />
                {c.header}
              </label>
              <MoveButton
                label="◀"
                title={`Move ${textOf(c.header)} left`}
                disabled={i === 0}
                onClick={() => onMove(c.key, -1)}
              />
              <MoveButton
                label="▶"
                title={`Move ${textOf(c.header)} right`}
                disabled={i === columns.length - 1}
                onClick={() => onMove(c.key, 1)}
              />
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

/** One arrow in the column picker. */
function MoveButton({
  label,
  title,
  disabled,
  onClick,
}: {
  label: string
  title: string
  disabled: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      title={title}
      aria-label={title}
      disabled={disabled}
      onClick={onClick}
      style={{
        border: 'none',
        background: 'none',
        color: disabled ? 'var(--text-muted)' : 'var(--text-secondary)',
        cursor: disabled ? 'default' : 'pointer',
        opacity: disabled ? 0.35 : 1,
        padding: '0 1px',
        fontSize: 9,
        lineHeight: 1,
      }}
    >
      {label}
    </button>
  )
}

/** The text of a column header, for a tooltip. Headers are usually strings;
 *  anything else gets a generic label rather than "[object Object]". */
function textOf(header: ReactNode): string {
  return typeof header === 'string' ? header : 'this column'
}

/**
 * Column visibility and order that survive a reload.
 *
 * localStorage, keyed by grid. UI-SPEC §5 says "persisted per user" and this is
 * per *browser* instead — the honest limitation is that choices do not follow
 * an operator to another machine. Doing it properly needs a preferences table
 * and an endpoint, which is not worth it while a controller has one operator;
 * when Phase 4 adds real multi-user accounts, this is the thing to move.
 */
export function useColumnPrefs(
  gridKey: string,
): [ColumnPrefs, (v: ColumnPrefs) => void] {
  const storageKey = `oonfee.columns.${gridKey}`
  const [prefs, setPrefs] = useState<ColumnPrefs>(() =>
    parsePrefs(localStorage.getItem(storageKey)),
  )
  const set = (v: ColumnPrefs) => {
    setPrefs(v)
    try {
      localStorage.setItem(storageKey, JSON.stringify(v))
    } catch {
      /* private mode, or a full quota: the grid still works for this session */
    }
  }
  return [prefs, set]
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

/**
 * A multi-select filter rail with live counts (UI-SPEC §5).
 *
 * `counted` says where the numbers came from, and it is required rather than
 * optional on purpose. The spec's whole point about this rail is that a count
 * taken from the loaded page is a lie; a rail that renders identically in both
 * cases makes that lie invisible, so the caller has to state which it is and
 * the component prints it.
 */
export function FilterRail({
  groups,
  counted,
}: {
  groups: {
    label: string
    options: { value: string; count: number }[]
    selected: string
    onChange: (v: string) => void
  }[]
  counted: 'all' | 'loaded'
}) {
  return (
    <Card title="Filters">
      {groups.map((g, i) => (
        <div key={g.label} style={{ marginTop: i === 0 ? 0 : 12 }}>
          <div
            style={{
              fontSize: 11,
              fontWeight: 600,
              color: 'var(--text-secondary)',
              marginBottom: 6,
            }}
          >
            {g.label}
          </div>
          <div style={{ display: 'grid', gap: 3 }}>
            <FilterOption
              label="All"
              count={g.options.reduce((n, o) => n + o.count, 0)}
              active={g.selected === ''}
              onClick={() => g.onChange('')}
            />
            {withSelected(g.options, g.selected).map((o) => (
              <FilterOption
                key={o.value}
                label={o.value}
                count={o.count}
                active={g.selected === o.value}
                onClick={() => g.onChange(o.value)}
              />
            ))}
          </div>
        </div>
      ))}
      <div style={{ fontSize: 10, color: 'var(--text-muted)', marginTop: 12 }}>
        {counted === 'all'
          ? 'Counts are over every matching row, not the page on screen.'
          : 'Counts are over the rows loaded here, which is everything this ' +
            'endpoint returns.'}
      </div>
    </Card>
  )
}

/**
 * Keep the selected option in the list even when nothing matches it.
 *
 * An option with no rows drops out of a count query, so selecting it makes it
 * disappear — and then the rail shows nothing highlighted above an empty grid,
 * with no indication that a filter is the reason. Observed exactly that on the
 * client list: the default "online" filter matched none of 14 clients, so the
 * screen said "0 of 14" beside a rail where no option looked selected.
 *
 * Showing it with a zero is both the explanation and the way back out.
 */
function withSelected(
  options: { value: string; count: number }[],
  selected: string,
) {
  if (selected === '' || options.some((o) => o.value === selected)) return options
  return [{ value: selected, count: 0 }, ...options]
}

function FilterOption({
  label,
  count,
  active,
  onClick,
}: {
  label: string
  count: number
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      style={{
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'center',
        padding: '3px 7px',
        borderRadius: 4,
        border: 'none',
        cursor: 'pointer',
        fontSize: 12,
        background: active ? 'var(--accent-soft)' : 'transparent',
        color: 'var(--text-primary)',
      }}
    >
      <span>{label}</span>
      <span className="num" style={{ color: 'var(--text-secondary)' }}>
        {count.toLocaleString()}
      </span>
    </button>
  )
}

/** Page controls with a page-size selector (UI-SPEC §5, default 100). */
export function Pager({
  total,
  limit,
  offset,
  onChange,
}: {
  total: number
  limit: number
  offset: number
  onChange: (limit: number, offset: number) => void
}) {
  const from = total === 0 ? 0 : offset + 1
  const to = Math.min(offset + limit, total)
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        padding: '6px 12px',
        borderTop: '1px solid var(--border)',
        fontSize: 11,
        color: 'var(--text-secondary)',
      }}
    >
      <span className="num">
        {from.toLocaleString()}–{to.toLocaleString()} of {total.toLocaleString()}
      </span>
      <div style={{ flex: 1 }} />
      <label style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
        Rows
        <select
          value={limit}
          onChange={(e) => onChange(Number(e.target.value), 0)}
          style={{
            background: 'var(--surface-0)',
            color: 'var(--text-primary)',
            border: '1px solid var(--border-strong)',
            borderRadius: 4,
            fontSize: 11,
            padding: '1px 4px',
          }}
        >
          {[50, 100, 250, 500, 1000].map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </label>
      <Button disabled={offset === 0} onClick={() => onChange(limit, Math.max(0, offset - limit))}>
        Previous
      </Button>
      <Button disabled={to >= total} onClick={() => onChange(limit, offset + limit)}>
        Next
      </Button>
    </div>
  )
}

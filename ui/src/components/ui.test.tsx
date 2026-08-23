import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import { useState } from 'react'
import { Banner, Button, DataGrid, FilterRail, Notice, Pager, SlideOver, Stat, Unknown, useColumnPrefs } from './ui'
import type { Column, ColumnPrefs } from './ui'
import { axisLabels, fmt, widenTo } from './Chart'

/**
 * Component tests for the shared grid.
 *
 * Every case here is anchored to something that actually broke, because a test
 * written from the implementation only asserts that the code does what it does.
 * The ones marked with a defect are from STATUS §5b, found by a human looking
 * at a screen — which is what this file exists to stop being the only way.
 *
 * happy-dom has no layout engine: getBoundingClientRect returns zeros and
 * clientHeight is 0. That rules out testing the row-height and sticky-header
 * defects here, and those are called out below rather than quietly skipped.
 */

interface Row {
  id: string
  name: string
  count: number | null
}

const rows: Row[] = [
  { id: 'a', name: 'alpha', count: 3 },
  { id: 'b', name: 'beta', count: null },
  { id: 'c', name: 'gamma', count: 0 },
]

const columns: Column<Row>[] = [
  { key: 'name', header: 'Name', required: true, render: (r) => r.name },
  { key: 'id', header: 'ID', render: (r) => r.id, sortBy: (r) => r.id },
  {
    key: 'count',
    header: 'Count',
    numeric: true,
    render: (r) => (r.count == null ? <Unknown why="never measured" /> : r.count),
    sortBy: (r) => r.count ?? -1,
  },
]

const noPrefs: ColumnPrefs = { hidden: [], order: [] }

describe('Banner', () => {
  it('leaves short notices unchanged', () => {
    render(<Banner>Nothing changed.</Banner>)
    expect(screen.queryByText(/Show details/)).toBeNull()
    expect(screen.getByText('Nothing changed.')).toBeTruthy()
  })

  it('truncates long notices behind native expandable details without losing content or semantics', () => {
    const notice = 'Topology evidence is incomplete. '.repeat(12)
    render(<div role="alert"><Banner>{notice}</Banner></div>)

    const alert = screen.getByRole('alert')
    const summary = within(alert).getByText('Show details').closest('summary') as HTMLElement
    const details = summary.closest('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    expect(summary.textContent?.length).toBeLessThan(notice.length)
    expect(alert.textContent).toContain(notice)
    summary.focus()
    expect(document.activeElement).toBe(summary)
    fireEvent.click(summary)
    expect(details.open).toBe(true)
    expect(within(alert).getByText('Hide details')).toBeTruthy()
  })

  it('keeps actionable prompts fully visible', () => {
    render(
      <Banner>
        {'Installing this optional capability changes the router. '.repeat(8)}
        <Button>Review capability</Button>
      </Banner>,
    )
    expect(screen.queryByText(/Show details/)).toBeNull()
    expect(screen.getByRole('button', { name: 'Review capability' })).toBeTruthy()
  })
})

describe('Notice', () => {
  it('keeps an authored summary visible and exposes details with native semantics', () => {
    render(
      <Notice
        tone="warning"
        component="Topology"
        summary="Some link evidence is unavailable."
        details="The LLDP source did not report a neighbour on lan3."
      />,
    )

    expect(screen.getByRole('group', { name: 'Warning: Topology' })).toBeTruthy()
    expect(screen.getByText('Some link evidence is unavailable.')).toBeTruthy()
    const toggle = screen.getByText('More information').closest('summary') as HTMLElement
    const disclosure = toggle.closest('details') as HTMLDetailsElement
    expect(toggle.tagName).toBe('SUMMARY')
    expect(toggle.getAttribute('aria-expanded')).toBe('false')
    expect(disclosure.open).toBe(false)

    toggle.focus()
    expect(document.activeElement).toBe(toggle)
    fireEvent.click(toggle)
    expect(disclosure.open).toBe(true)
    expect(screen.getByText('Hide information').getAttribute('aria-expanded')).toBe('true')
    expect(screen.getByText('The LLDP source did not report a neighbour on lan3.')).toBeTruthy()
  })

  it('opens an active capability plan while keeping consent actions outside it', () => {
    function CapabilityNotice() {
      const [reviewed, setReviewed] = useState(false)
      return (
        <Notice
          tone="accent"
          component="LLDP capability"
          summary="Adds measured wired-neighbour discovery."
          details={reviewed ? 'Install lldpd (84 KiB); rollback removes only this addition.' : 'No router change occurs until a plan is reviewed and accepted.'}
          defaultOpen={reviewed}
          actions={reviewed
            ? <><Button kind="primary">Install capability</Button><Button onClick={() => setReviewed(false)}>Cancel</Button></>
            : <Button onClick={() => setReviewed(true)}>Review</Button>}
        />
      )
    }

    render(<CapabilityNotice />)
    const review = screen.getByRole('button', { name: 'Review' })
    expect(review.closest('details')).toBeNull()
    expect((screen.getByText('More information').closest('details') as HTMLDetailsElement).open).toBe(false)

    fireEvent.click(review)
    const install = screen.getByRole('button', { name: 'Install capability' })
    const cancel = screen.getByRole('button', { name: 'Cancel' })
    expect((screen.getByText('Hide information').closest('details') as HTMLDetailsElement).open).toBe(true)
    expect(install.closest('details')).toBeNull()
    expect(cancel.closest('details')).toBeNull()
    expect(screen.getByText(/Install lldpd/)).toBeTruthy()

    fireEvent.click(cancel)
    expect((screen.getByText('More information').closest('details') as HTMLDetailsElement).open).toBe(false)
  })
})

describe('SlideOver', () => {
  it('keeps keyboard focus inside, preserves it across renders, and restores the opener', () => {
    function Example() {
      const [open, setOpen] = useState(false)
      const [checked, setChecked] = useState(false)
      return (
        <>
          <button onClick={() => setOpen(true)}>Open panel</button>
          {open && (
            <SlideOver title="Policy details" onClose={() => setOpen(false)}>
              <label>
                <input
                  type="checkbox"
                  checked={checked}
                  onChange={(event) => setChecked(event.target.checked)}
                />
                Destination
              </label>
              <button>Last action</button>
            </SlideOver>
          )}
        </>
      )
    }

    render(<Example />)
    const opener = screen.getByText('Open panel')
    opener.focus()
    fireEvent.click(opener)

    const dialog = screen.getByRole('dialog', { name: 'Policy details' })
    const close = within(dialog).getByRole('button', { name: 'Close' })
    const checkbox = within(dialog).getByRole('checkbox')
    const last = within(dialog).getByText('Last action')
    expect(document.activeElement).toBe(close)

    last.focus()
    fireEvent.keyDown(window, { key: 'Tab' })
    expect(document.activeElement).toBe(close)
    fireEvent.keyDown(window, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(last)

    checkbox.focus()
    fireEvent.click(checkbox)
    expect(document.activeElement).toBe(checkbox)

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(opener)
  })
})

function headers(): string[] {
  return screen
    .getAllByRole('columnheader')
    .map((th) => th.textContent?.replace(/[↑↓]/g, '').trim() ?? '')
}

function bodyRows(): HTMLElement[] {
  return screen.getAllByRole('row').filter((r) => r.hasAttribute('data-row'))
}

describe('DataGrid', () => {
  it('renders every row while the grid is small', () => {
    render(<DataGrid rows={rows} columns={columns} rowKey={(r) => r.id} />)
    expect(bodyRows()).toHaveLength(3)
  })

  // A grid of 13,000 clients must not put 13,000 rows in the DOM. There is no
  // layout here, so the window resolves to its overscan — which is the point:
  // what is asserted is that windowing ENGAGED, not the exact count.
  it('windows a large grid instead of rendering all of it', () => {
    const many = Array.from({ length: 400 }, (_, i) => ({
      id: `r${i}`,
      name: `row ${i}`,
      count: i,
    }))
    render(<DataGrid rows={many} columns={columns} rowKey={(r) => r.id} />)
    const drawn = bodyRows().length
    expect(drawn).toBeGreaterThan(0)
    expect(drawn).toBeLessThan(many.length)
  })

  it('hides a column the preferences hide, and keeps required ones', () => {
    render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(r) => r.id}
        prefs={{ hidden: ['id', 'name'], order: [] }}
        onPrefsChange={() => {}}
      />,
    )
    // `name` is required, so hiding it is not honoured: a grid of attributes
    // belonging to nothing is worse than a grid with an unwanted column.
    expect(headers()).toEqual(['Name', 'Count'])
  })

  it('applies a saved column order', () => {
    render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(r) => r.id}
        prefs={{ hidden: [], order: ['count', 'name', 'id'] }}
        onPrefsChange={() => {}}
      />,
    )
    expect(headers()).toEqual(['Count', 'Name', 'ID'])
  })

  // The reorder controls must hand back the FULL key list, hidden columns
  // included. Rewriting only the visible ones loses the hidden ones' places, so
  // unhiding a column later drops it somewhere the operator never chose.
  it('reorders through the picker and keeps hidden columns in the order', () => {
    const onPrefsChange = vi.fn()
    render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(r) => r.id}
        prefs={{ hidden: ['id'], order: [] }}
        onPrefsChange={onPrefsChange}
      />,
    )
    fireEvent.click(screen.getByText(/Customize columns/))
    fireEvent.click(screen.getByLabelText('Move Name right'))

    expect(onPrefsChange).toHaveBeenCalledTimes(1)
    const next = onPrefsChange.mock.calls[0][0] as ColumnPrefs
    expect(next.order).toContain('id')
    expect(next.order).toHaveLength(columns.length)
    expect(next.hidden).toEqual(['id'])
  })

  // fireEvent.dragStart dispatches the event whatever the DOM says, so every
  // other drag test here passes on a header a real mouse could never pick up.
  // The `draggable` attribute is the thing a browser actually consults, so it
  // gets asserted directly.
  //
  // This is not hypothetical: the Devices grid shipped without column prefs, so
  // its headers were draggable={false} and dragging one did nothing at all —
  // no reorder, no picker, not even the tooltip that says dragging is possible.
  // Found by a person trying it, which is the only way it could have been.
  it('marks headers draggable only when reordering is actually wired up', () => {
    const { unmount } = render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(r) => r.id}
        prefs={noPrefs}
        onPrefsChange={vi.fn()}
      />,
    )
    for (const th of screen.getAllByRole('columnheader')) {
      expect(th.getAttribute('draggable')).toBe('true')
      expect(th.getAttribute('title')).toMatch(/drag/i)
    }
    unmount()

    // And a grid with no prefs must not advertise a drag it cannot perform.
    render(<DataGrid rows={rows} columns={columns} rowKey={(r) => r.id} />)
    for (const th of screen.getAllByRole('columnheader')) {
      expect(th.getAttribute('draggable')).not.toBe('true')
      expect(th.getAttribute('title')).toBeNull()
    }
  })

  // A drag that lands must not also sort the column it landed on. Getting this
  // wrong means every reorder silently re-sorts the grid.
  it('does not sort the column a drag was dropped onto', () => {
    const onPrefsChange = vi.fn()
    render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(r) => r.id}
        prefs={noPrefs}
        onPrefsChange={onPrefsChange}
      />,
    )
    const [nameTh, idTh] = screen.getAllByRole('columnheader')
    fireEvent.dragStart(nameTh, {
      dataTransfer: { effectAllowed: '', setData: vi.fn() },
    })
    fireEvent.drop(idTh)
    fireEvent.click(within(idTh).getByRole('button')) // some browsers emit this after a drop

    expect(onPrefsChange).toHaveBeenCalledTimes(1)
    // No sort indicator anywhere: the click was swallowed.
    expect(headers().join(' ')).not.toMatch(/[↑↓]/)
  })

  it('sorts through a native keyboard button and reports the direction', () => {
    render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(r) => r.id}
        prefs={noPrefs}
        onPrefsChange={() => {}}
      />,
    )
    const idHeader = () => screen.getAllByRole('columnheader')[1]
    const idButton = () => within(idHeader()).getByRole('button')
    expect(idHeader().getAttribute('aria-sort')).toBe('none')
    fireEvent.click(idButton())
    expect(bodyRows()[0].textContent).toContain('alpha')
    expect(idHeader().getAttribute('aria-sort')).toBe('ascending')
    fireEvent.click(idButton())
    expect(bodyRows()[0].textContent).toContain('gamma')
    expect(idHeader().getAttribute('aria-sort')).toBe('descending')
    expect(idHeader().textContent).toMatch(/↓/)
  })

  it('opens an actionable row from Enter or Space without stealing child controls', () => {
    const open = vi.fn()
    const child = vi.fn()
    const interactive: Column<Row>[] = [
      columns[0],
      {
        key: 'action',
        header: 'Action',
        render: () => <button onClick={child}>Child action</button>,
      },
    ]
    render(
      <DataGrid
        rows={[rows[0]]}
        columns={interactive}
        rowKey={(row) => row.id}
        onRowClick={open}
      />,
    )
    const row = bodyRows()[0]
    expect(row.tabIndex).toBe(0)

    row.focus()
    fireEvent.keyDown(row, { key: 'Enter' })
    fireEvent.keyDown(row, { key: ' ' })
    expect(open).toHaveBeenCalledTimes(2)

    const button = within(row).getByRole('button', { name: 'Child action' })
    fireEvent.click(button)
    fireEvent.keyDown(button, { key: 'Enter' })
    expect(child).toHaveBeenCalledTimes(1)
    expect(open).toHaveBeenCalledTimes(2)
  })

  it('optionally names the table and exposes total row positions', () => {
    render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(row) => row.id}
        tableLabel="Inventory devices"
        totalRows={1000}
        rowOffset={40}
      />,
    )
    const table = screen.getByRole('table', { name: 'Inventory devices' })
    expect(table.getAttribute('aria-rowcount')).toBe('1001')
    expect(bodyRows().map((row) => row.getAttribute('aria-rowindex'))).toEqual([
      '42', '43', '44',
    ])
  })

  // UI-SPEC §7: an unknown value and a zero are different claims, and a grid
  // that renders both as blank-ish makes the difference unrecoverable.
  it('distinguishes an unknown value from a zero', () => {
    render(<DataGrid rows={rows} columns={columns} rowKey={(r) => r.id} />)
    const [, beta, gamma] = bodyRows()
    expect(within(beta).getByRole('button', { name: 'Unknown: never measured' })).toBeTruthy()
    expect(gamma.textContent).toContain('0')
    expect(within(gamma).queryByRole('button', { name: /Unknown/ })).toBeNull()
  })

  it('shows the empty message rather than an empty table', () => {
    render(
      <DataGrid
        rows={[]}
        columns={columns}
        rowKey={(r) => r.id}
        empty="Nothing here yet."
      />,
    )
    expect(screen.getByText('Nothing here yet.')).toBeTruthy()
    expect(screen.queryAllByRole('columnheader')).toHaveLength(0)
  })
})

describe('FilterRail', () => {
  // DEFECT (§5b): the default "online" filter matched none of 14 clients, so
  // the option vanished from the rail — leaving nothing highlighted above an
  // empty grid, with no indication that a filter was the reason.
  it('keeps the selected option visible when nothing matches it', () => {
    render(
      <FilterRail
        counted="all"
        groups={[
          {
            label: 'Presence',
            options: [{ value: 'offline', count: 4 }],
            selected: 'online',
            onChange: () => {},
          },
        ]}
      />,
    )
    expect(screen.getByText('online')).toBeTruthy()
    expect(screen.getByText('offline')).toBeTruthy()
  })

  it('says whether counts cover everything or only what is loaded', () => {
    const { rerender } = render(
      <FilterRail
        counted="all"
        groups={[
          { label: 'Scope', options: [], selected: '', onChange: () => {} },
        ]}
      />,
    )
    expect(screen.getByText(/every matching row/)).toBeTruthy()
    rerender(
      <FilterRail
        counted="loaded"
        groups={[
          { label: 'Scope', options: [], selected: '', onChange: () => {} },
        ]}
      />,
    )
    expect(screen.getByText(/rows loaded here/)).toBeTruthy()
  })

  it('labels each option group and exposes its selected option', () => {
    render(
      <FilterRail
        counted="all"
        groups={[
          {
            label: 'Presence',
            options: [{ value: 'online', count: 3 }],
            selected: 'online',
            onChange: () => {},
          },
          {
            label: 'Connection',
            options: [{ value: 'wireless', count: 2 }],
            selected: '',
            onChange: () => {},
          },
        ]}
      />,
    )
    const presence = screen.getByRole('group', { name: 'Presence' })
    const connection = screen.getByRole('group', { name: 'Connection' })
    expect(within(presence).getByRole('button', { name: /online/i }).getAttribute('aria-pressed')).toBe('true')
    expect(within(presence).getByRole('button', { name: /All/i }).getAttribute('aria-pressed')).toBe('false')
    expect(within(connection).getByRole('button', { name: /All/i }).getAttribute('aria-pressed')).toBe('true')
  })
})

describe('Unknown', () => {
  it('exposes the reason to assistive technology and on focus or click', () => {
    render(<Unknown why="the source did not answer" />)
    const trigger = screen.getByRole('button', {
      name: 'Unknown: the source did not answer',
    })
    expect(trigger.getAttribute('aria-expanded')).toBe('false')

    fireEvent.focus(trigger)
    expect(screen.getByRole('tooltip').textContent).toBe('the source did not answer')
    expect(trigger.getAttribute('aria-expanded')).toBe('true')

    fireEvent.keyDown(trigger, { key: 'Escape' })
    expect(screen.queryByRole('tooltip')).toBeNull()

    fireEvent.click(trigger)
    expect(screen.getByRole('tooltip').textContent).toBe('the source did not answer')
    fireEvent.blur(trigger)
    expect(screen.queryByRole('tooltip')).toBeNull()
  })
})

describe('Pager', () => {
  it('counts from one and does not run past the total', () => {
    render(<Pager total={13000} limit={100} offset={0} onChange={() => {}} />)
    expect(screen.getByText(/1–100 of 13,000/)).toBeTruthy()
  })

  it('reports an empty result as 0 rather than 1–0', () => {
    render(<Pager total={0} limit={100} offset={0} onChange={() => {}} />)
    expect(screen.getByText(/0 of 0/)).toBeTruthy()
  })
})

describe('Stat', () => {
  // A count that excludes something has to say so next to itself. The dashboard
  // scopes "Devices on the LAN" to this network, and without the sub-line the
  // number is simply smaller than the previous build's with nothing to
  // distinguish a correct rescoping from lost devices.
  it('renders the sub-line naming what a number leaves out', () => {
    render(<Stat label="Devices on the LAN" value={3} sub="7 upstream not counted" />)
    expect(screen.getByText('7 upstream not counted')).toBeTruthy()
  })

  it('omits the sub-line entirely when there is nothing to add', () => {
    const { container } = render(<Stat label="Devices online" value="2/2" />)
    expect(container.textContent).toBe('Devices online2/2')
  })
})

describe('useColumnPrefs', () => {
  // Reaching for localStorage THROWS in some browsers — Safari's private mode
  // historically, and any profile with site data blocked. The read runs inside
  // a useState initialiser, so an unguarded access does not lose a preference:
  // it unmounts the screen. Found by the test environment, which supplies a
  // localStorage object with none of the Storage methods on it.
  it('renders when localStorage is unavailable', () => {
    const real = globalThis.localStorage
    Object.defineProperty(globalThis, 'localStorage', {
      configurable: true,
      get() {
        throw new Error('access denied')
      },
    })
    try {
      expect(() =>
        render(<Grid prefs />),
      ).not.toThrow()
    } finally {
      Object.defineProperty(globalThis, 'localStorage', {
        value: real,
        configurable: true,
      })
    }
  })

  it('remembers hidden columns across a remount', () => {
    const { unmount } = render(<Grid prefs />)
    fireEvent.click(screen.getByText(/Customize columns/))
    fireEvent.click(screen.getByLabelText('ID'))
    expect(headers()).toEqual(['Name', 'Count'])

    unmount()
    render(<Grid prefs />)
    expect(headers()).toEqual(['Name', 'Count'])
  })

  // A column hidden and then reordered must come back where it was put, not
  // wherever the built-in order happens to place it.
  it('remembers where a hidden column belongs', () => {
    const { unmount } = render(<Grid prefs />)
    fireEvent.click(screen.getByText(/Customize columns/))
    // Move Count to the front while everything is visible, then hide it.
    fireEvent.click(screen.getByLabelText('Move Count left'))
    fireEvent.click(screen.getByLabelText('Move Count left'))
    expect(headers()).toEqual(['Count', 'Name', 'ID'])
    fireEvent.click(screen.getByLabelText('Count'))
    expect(headers()).toEqual(['Name', 'ID'])

    unmount()
    render(<Grid prefs />)
    fireEvent.click(screen.getByText(/Customize columns/))
    fireEvent.click(screen.getByLabelText('Count'))
    expect(headers()).toEqual(['Count', 'Name', 'ID'])
  })
})

/** The grid under test, wired to the real persistence hook. */
function Grid({ prefs }: { prefs: boolean }) {
  const [colPrefs, setColPrefs] = useColumnPrefs('test-grid')
  return (
    <DataGrid
      rows={rows}
      columns={columns}
      rowKey={(r) => r.id}
      prefs={prefs ? colPrefs : undefined}
      onPrefsChange={prefs ? setColPrefs : undefined}
    />
  )
}

describe('axisLabels', () => {
  // The user-visible property, and the one a formatter test cannot reach: the
  // AXIS has to tell the formatter how far apart its ticks are. fmt.percent
  // passed its own tests the whole time while the chart called it with no step,
  // so every tick still collapsed to the same string on screen.
  it('gives the formatter the tick spacing, so labels stay distinct', () => {
    const labels = axisLabels([62.8, 63.0, 63.2], fmt.percent)
    expect(new Set(labels).size).toBe(3)
    expect(labels[0]).toBe('62.8%')
  })

  it('does not add precision when the ticks are far apart', () => {
    expect(axisLabels([20, 40, 60], fmt.percent)).toEqual(['20%', '40%', '60%'])
  })

  // A single tick has no spacing to derive anything from, and must not divide
  // by a gap that does not exist.
  it('survives an axis with one tick', () => {
    expect(axisLabels([63.2], fmt.percent)).toEqual(['63%'])
  })
})

describe('widenTo', () => {
  // uPlot fits the axis to the data, so a series that barely moves is magnified
  // until its rounding noise fills the chart. Channel occupancy sitting between
  // 63.020% and 63.030% was drawn as a dramatic climb, with three-decimal
  // labels too wide for the gutter and clipped on screen to "i3.030%".
  it('stops a flat series being magnified into a trend', () => {
    const [lo, hi] = widenTo(63.02, 63.03, 1)
    expect(hi - lo).toBeCloseTo(1)
    // Still centred on the data, so the flat line sits in the middle.
    expect((lo + hi) / 2).toBeCloseTo(63.025)
    // And the labels that come out of it are narrow enough to render.
    expect(axisLabels([lo, (lo + hi) / 2, hi], fmt.percent).every((s) => s.length <= 7)).toBe(true)
  })

  it('leaves a range that is already wide enough alone', () => {
    expect(widenTo(60, 80, 1)).toEqual([60, 80])
  })

  // Percentages and rates cannot go negative, so widening near zero must not
  // draw axis room for values that cannot occur.
  it('does not open a negative floor', () => {
    const [lo, hi] = widenTo(0.1, 0.2, 1)
    expect(lo).toBe(0)
    expect(hi).toBeGreaterThanOrEqual(1)
  })
})

describe('fmt.percent', () => {
  // The property that matters: two neighbouring ticks must not render the same
  // string. Decimals used to be chosen from the VALUE's magnitude — one below
  // 10, none above — so an axis spanning 0.6 points around 63 labelled every
  // tick "63%". Seen on a survey chart holding two samples: two labels reading
  // 63% at different heights with a line sloping between them, which a reader
  // cannot tell from a broken axis.
  it('keeps neighbouring ticks distinguishable', () => {
    const step = 0.2
    const ticks = [62.8, 63.0, 63.2].map((v) => fmt.percent(v, step))
    expect(new Set(ticks).size).toBe(ticks.length)
  })

  // And does not spend decimals it does not need.
  it('drops decimals when the ticks are far apart', () => {
    expect(fmt.percent(63.2, 20)).toBe('63%')
    expect(fmt.percent(63.2, 5)).toBe('63%')
  })

  // With no axis to consult — a tooltip, a table cell — one reading stands
  // alone and the magnitude rule is the sensible fallback.
  it('falls back to the magnitude rule with no step', () => {
    expect(fmt.percent(63.2)).toBe('63%')
    expect(fmt.percent(6.32)).toBe('6.3%')
  })

  // A degenerate axis must not ask for endless precision: a series that flat is
  // better described by a flat line than by six decimals.
  it('caps the decimals it will ask for', () => {
    expect(fmt.percent(63.20000001, 0.0000001).length).toBeLessThanOrEqual(8)
  })
})

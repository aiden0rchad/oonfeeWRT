import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import { DataGrid, FilterRail, Pager, Stat, Unknown, useColumnPrefs } from './ui'
import type { Column, ColumnPrefs } from './ui'

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
    fireEvent.dragStart(nameTh)
    fireEvent.drop(idTh)
    fireEvent.click(idTh) // some browsers emit this after a drop

    expect(onPrefsChange).toHaveBeenCalledTimes(1)
    // No sort indicator anywhere: the click was swallowed.
    expect(headers().join(' ')).not.toMatch(/[↑↓]/)
  })

  it('still sorts on an ordinary header click', () => {
    render(
      <DataGrid
        rows={rows}
        columns={columns}
        rowKey={(r) => r.id}
        prefs={noPrefs}
        onPrefsChange={() => {}}
      />,
    )
    // By role, not by text: after the first click the header reads "ID ↑", so
    // a text lookup finds it once and then cannot.
    const idHeader = () => screen.getAllByRole('columnheader')[1]
    fireEvent.click(idHeader())
    expect(bodyRows()[0].textContent).toContain('alpha')
    fireEvent.click(idHeader())
    expect(bodyRows()[0].textContent).toContain('gamma')
    // The direction has to be visible, or a sorted grid looks unsorted.
    expect(idHeader().textContent).toMatch(/↓/)
  })

  // UI-SPEC §7: an unknown value and a zero are different claims, and a grid
  // that renders both as blank-ish makes the difference unrecoverable.
  it('distinguishes an unknown value from a zero', () => {
    render(<DataGrid rows={rows} columns={columns} rowKey={(r) => r.id} />)
    const [, beta, gamma] = bodyRows()
    expect(within(beta).getByTitle('never measured')).toBeTruthy()
    expect(gamma.textContent).toContain('0')
    expect(within(gamma).queryByTitle('never measured')).toBeNull()
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

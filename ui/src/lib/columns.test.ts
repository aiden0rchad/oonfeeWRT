/**
 * The rules behind column ordering and stored preferences.
 *
 * These started life as `columns.check.ts`, a script run under plain node
 * because the UI had no test runner. It has one now, and keeping two parallel
 * ways to test the same code is how the two stop agreeing — so this is the
 * same assertions, in the runner.
 *
 * They cover what is easy to get wrong and invisible when it is: the asymmetry
 * of moving right versus left, hidden columns keeping their place, and old
 * stored preferences surviving a format change.
 */
import { expect, it } from 'vitest'
import { moveColumn, orderColumns, parsePrefs } from './columns'

function check(name: string, got: unknown, want: unknown) {
  it(name, () => expect(got).toEqual(want))
}

const cols = [{ key: 'a' }, { key: 'b' }, { key: 'c' }, { key: 'd' }]
const keys = (cs: { key: string }[]) => cs.map((c) => c.key)

// --- orderColumns ---

check('no saved order keeps the built-in one', keys(orderColumns(cols, [])), [
  'a',
  'b',
  'c',
  'd',
])

check(
  'a saved order is applied',
  keys(orderColumns(cols, ['d', 'c', 'b', 'a'])),
  ['d', 'c', 'b', 'a'],
)

// A later build adding a column must still show it, at the end, rather than
// dropping it because storage predates it.
check(
  'a column the saved order does not know still appears',
  keys(orderColumns(cols, ['c', 'a'])),
  ['c', 'a', 'b', 'd'],
)

// And a build REMOVING a column must not break the order it left behind.
check(
  'a saved key for a column that no longer exists is skipped',
  keys(orderColumns(cols, ['gone', 'c', 'a'])),
  ['c', 'a', 'b', 'd'],
)

// A duplicated key — a hand-edited store — must not duplicate the column.
check(
  'a duplicated saved key does not duplicate the column',
  keys(orderColumns(cols, ['b', 'b', 'a'])),
  ['b', 'a', 'c', 'd'],
)

// --- moveColumn -------------------------------------------------------------

// The asymmetry that makes "drag right by one" silently do nothing if you get
// it wrong: removing the dragged key first shifts the target left into the
// slot the dragged column just left.
check('move right by one', moveColumn(['a', 'b', 'c', 'd'], 'a', 'b'), [
  'b',
  'a',
  'c',
  'd',
])
check('move left by one', moveColumn(['a', 'b', 'c', 'd'], 'b', 'a'), [
  'b',
  'a',
  'c',
  'd',
])
check('move right across several', moveColumn(['a', 'b', 'c', 'd'], 'a', 'd'), [
  'b',
  'c',
  'd',
  'a',
])
check('move left across several', moveColumn(['a', 'b', 'c', 'd'], 'd', 'a'), [
  'd',
  'a',
  'b',
  'c',
])
check('onto itself is a no-op', moveColumn(['a', 'b', 'c'], 'b', 'b'), [
  'a',
  'b',
  'c',
])
check('an unknown target is a no-op', moveColumn(['a', 'b', 'c'], 'a', 'z'), [
  'a',
  'b',
  'c',
])
check('an unknown source is a no-op', moveColumn(['a', 'b', 'c'], 'z', 'a'), [
  'a',
  'b',
  'c',
])

// Round-tripping must land where it started, or repeated nudges drift. This is
// exactly what the picker's ◀ ▶ buttons do.
check(
  'right then left returns to the start',
  moveColumn(moveColumn(['a', 'b', 'c'], 'a', 'b'), 'a', 'b'),
  ['a', 'b', 'c'],
)

// --- parsePrefs -------------------------------------------------------------

check('nothing stored', parsePrefs(null), { hidden: [], order: [] })
check('empty string', parsePrefs(''), { hidden: [], order: [] })
check('malformed JSON is forgotten, not thrown', parsePrefs('{oops'), {
  hidden: [],
  order: [],
})

// The migration: the first format was a bare array of hidden keys. Someone who
// hid four columns must not get them all back because a later build started
// storing an order alongside.
check('the old bare-array format migrates', parsePrefs('["mac","ip"]'), {
  hidden: ['mac', 'ip'],
  order: [],
})

check(
  'the current format round-trips',
  parsePrefs('{"hidden":["mac"],"order":["name","ip"]}'),
  { hidden: ['mac'], order: ['name', 'ip'] },
)

// Storage is attacker-adjacent only in the sense that it is user-editable, but
// a number where a string belongs must not reach the grid as a column key.
check(
  'non-strings are dropped rather than trusted',
  parsePrefs('{"hidden":[1,"mac",null],"order":"nope"}'),
  { hidden: ['mac'], order: [] },
)

check('a JSON scalar is not preferences', parsePrefs('42'), {
  hidden: [],
  order: [],
})

import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'
import { checkScreenshots } from './check-screenshots.mjs'

// Copy an existing JPEG into isolated fixtures; no image codec or capture is needed.
const screenshot = await readFile(new URL('../images/dashboard-overview.jpg', import.meta.url))

async function fixture(t) {
  const root = await mkdtemp(join(tmpdir(), 'oonfeewrt-screenshot-check-'))
  t.after(() => rm(root, { recursive: true, force: true }))
  const write = async (path, content) => {
    const target = join(root, path)
    await mkdir(join(target, '..'), { recursive: true })
    await writeFile(target, content)
  }
  await write('README.md', '<img src="docs/public/screenshots/dashboard-dark.jpg">')
  await write('docs/guide.md', '<DocScreenshot\n src="dashboard"\n :width="1416" :height="925"\n alt="Dashboard" />')
  await write('docs/public/screenshots/dashboard-dark.jpg', screenshot)
  return { root, write }
}

test('validates dark-only screenshots and README, ignoring generated Markdown and code', async (t) => {
  const { root, write } = await fixture(t)
  await write('docs/node_modules/example.md', '<DocScreenshot src="missing" />')
  await write('docs/.vitepress/cache/example.md', '<DocScreenshot src="missing" />')
  await write('docs/examples.md', [
    'Inline example: `<DocScreenshot src="<screen>" />`.',
    '```vue', '<DocScreenshot src="not-an-embed" />', '```',
    '~~~vue', '<DocScreenshot src="also-not-an-embed" />', '~~~',
  ].join('\n'))
  assert.deepEqual(await checkScreenshots(root), { errors: [], references: 1, screenshots: 1 })
})

test('missing dark screenshot fails when referenced only by README', async (t) => {
  const { root, write } = await fixture(t)
  await write('docs/guide.md', '# No screenshot components')
  await rm(join(root, 'docs/public/screenshots/dashboard-dark.jpg'))
  assert.match((await checkScreenshots(root)).errors.join('\n'), /dashboard-dark\.jpg: missing screenshot/)
})

test('rejects truncated JPEG segments', async (t) => {
  const { root, write } = await fixture(t)
  await write('docs/public/screenshots/dashboard-dark.jpg', Buffer.from([0xff, 0xd8, 0xff, 0xe0, 0x00, 0x20]))
  assert.match((await checkScreenshots(root)).errors.join('\n'), /invalid JPEG segment length/)
})

test('rejects non-JPEG files and broken legacy README paths', async (t) => {
  const { root, write } = await fixture(t)
  await write('docs/public/screenshots/dashboard-dark.jpg', 'not an image')
  await write('README.md', '![Old screenshot](docs/images/removed.jpg)')
  const { errors } = await checkScreenshots(root)
  assert.equal(errors.length, 2)
  assert.match(errors.join('\n'), /not a JPEG file/)
  assert.match(errors.join('\n'), /docs\/images\/removed\.jpg: missing screenshot/)
})

test('invalid and dynamic stems fail with their documentation source location', async (t) => {
  const { root, write } = await fixture(t)
  await write('docs/guide.md', '<DocScreenshot src="../escape" />\n<DocScreenshot :src="variable" />')
  const { errors } = await checkScreenshots(root)
  assert.equal(errors.length, 2)
  assert.match(errors[0], /docs\/guide\.md:1:/)
  assert.match(errors[1], /docs\/guide\.md:2:/)
})

test('compares each reference against actual JPEG dimensions with its source location', async (t) => {
  const { root, write } = await fixture(t)
  await write('docs/guide.md', [
    '# Screenshots',
    '<DocScreenshot src="dashboard" :width="1416" :height="925" />',
    '<DocScreenshot src="dashboard" :width="1620" :height="959" />',
    '<DocScreenshot src="dashboard" :width="925" :height="1416" />',
  ].join('\n'))
  const result = await checkScreenshots(root)
  assert.equal(result.references, 3)
  assert.equal(result.screenshots, 1)
  assert.deepEqual(result.errors, [
    'docs/guide.md:3: declared 1620x959 does not match docs/public/screenshots/dashboard-dark.jpg (1416x925)',
    'docs/guide.md:4: declared 925x1416 does not match docs/public/screenshots/dashboard-dark.jpg (1416x925)',
  ])
})

test('accepts static numeric attributes across quote styles, line breaks, and binding forms', async (t) => {
  const { root, write } = await fixture(t)
  await write('docs/guide.md', [
    '<DocScreenshot caption="A > B with :width=\'100\' inside text"',
    " src='dashboard' v-bind:width='1416'",
    " :height = '925' />",
    '<DocScreenshot src="dashboard" width="1416" height="925" />',
  ].join('\n'))
  assert.deepEqual(await checkScreenshots(root), { errors: [], references: 2, screenshots: 1 })
})

test('rejects missing, dynamic, invalid, and duplicate dimension attributes', async (t) => {
  const { root, write } = await fixture(t)
  const dimensions = [
    '', ':width="1416"', ':width="image.width" :height="925"',
    ':width="0" :height="925"', ':width="-1416" :height="925"',
    ':width="1416.5" :height="925"', ':width="65536" :height="925"',
    ':width="1416" :height="NaN"', ':width="1416" :height="Infinity"',
    ':width="1416" width="1416" :height="925"',
    ':width="1416" :height="925" v-bind:height="925"',
  ]
  await write('docs/guide.md', dimensions.map((value) => `<DocScreenshot src="dashboard" ${value} />`).join('\n'))
  const { errors, references } = await checkScreenshots(root)
  assert.equal(references, dimensions.length)
  assert.equal(errors.length, dimensions.length)
  errors.forEach((error, index) => {
    assert.ok(error.startsWith(`docs/guide.md:${index + 1}:`))
    assert.match(error, /static positive-integer width and height/)
  })
})

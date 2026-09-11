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
  await write('docs/guide.md', '<DocScreenshot\n src="dashboard"\n alt="Dashboard" />')
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

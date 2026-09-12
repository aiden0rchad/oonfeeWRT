import { readFile, readdir } from 'node:fs/promises'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const stemPattern = /^[a-z0-9]+(?:-[a-z0-9]+)*$/

function jpegDimensions(bytes) {
  if (bytes.length < 4 || bytes.readUInt16BE(0) !== 0xffd8) throw new Error('not a JPEG file')
  let offset = 2
  while (offset < bytes.length) {
    if (bytes[offset++] !== 0xff) throw new Error('invalid JPEG marker')
    while (bytes[offset] === 0xff) offset++
    const marker = bytes[offset++]
    if (marker === 0xda || marker === 0xd9) break
    if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue
    if (offset + 2 > bytes.length) throw new Error('truncated JPEG segment')
    const length = bytes.readUInt16BE(offset)
    if (length < 2 || offset + length > bytes.length) throw new Error('invalid JPEG segment length')
    // Start-of-frame headers carry dimensions for baseline and progressive JPEGs.
    if (marker >= 0xc0 && marker <= 0xcf && ![0xc4, 0xc8, 0xcc].includes(marker)) {
      if (length < 8) throw new Error('truncated JPEG frame header')
      const height = bytes.readUInt16BE(offset + 3)
      const width = bytes.readUInt16BE(offset + 5)
      if (!width || !height) throw new Error('invalid JPEG dimensions')
      return { width, height }
    }
    offset += length
  }
  throw new Error('missing JPEG frame header')
}

function withoutCode(markdown) {
  const blank = (value) => value.replace(/[^\n]/g, ' ')
  return markdown
    .replace(/(^ {0,3}(`{3,}|~{3,})[^\n]*\n)[\s\S]*?^ {0,3}\2[ \t]*(?:\n|$)/gm, blank)
    .replace(/(`+)[\s\S]*?\1/g, blank)
}

async function markdownFiles(directory) {
  const files = []
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name === '.vitepress') continue
    const path = join(directory, entry.name)
    if (entry.isDirectory()) files.push(...await markdownFiles(path))
    else if (entry.isFile() && entry.name.endsWith('.md')) files.push(path)
  }
  return files.sort()
}

export async function checkScreenshots(repoRoot) {
  const errors = []
  const stems = new Set()
  const files = new Set()
  const declaredDimensions = []
  const actualDimensions = new Map()
  let references = 0
  for (const path of await markdownFiles(join(repoRoot, 'docs'))) {
    const markdown = withoutCode(await readFile(path, 'utf8'))
    for (const match of markdown.matchAll(/<DocScreenshot\b((?:"[^"]*"|'[^']*'|[^'">])*)>/gs)) {
      const attributes = [...match[1].matchAll(/([^\s=]+)\s*=\s*(?:"([^"]*)"|'([^']*)')/g)]
        .map((attribute) => [attribute[1], attribute[2] ?? attribute[3]])
      const source = attributes.find(([name]) => name === 'src')?.[1]
      const location = `${relative(repoRoot, path)}:${markdown.slice(0, match.index).split('\n').length}`
      if (!source || !stemPattern.test(source)) errors.push(`${location}: DocScreenshot needs a static lowercase, hyphen-separated src stem`)
      else {
        stems.add(source)
        const dimension = (name) => {
          const values = attributes.filter(([key]) => [name, `:${name}`, `v-bind:${name}`].includes(key))
          return values.length === 1 && /^[1-9]\d*$/.test(values[0][1]) && Number(values[0][1]) <= 65535
            ? Number(values[0][1]) : null
        }
        const width = dimension('width')
        const height = dimension('height')
        if (width == null || height == null) {
          errors.push(`${location}: DocScreenshot needs static positive-integer width and height attributes, for example :width="1600" :height="1000"`)
        } else {
          declaredDimensions.push({ path: `docs/public/screenshots/${source}-dark.jpg`, location, width, height })
        }
      }
      references++
    }
  }
  const readme = await readFile(join(repoRoot, 'README.md'), 'utf8')
  for (const match of readme.matchAll(/\bdocs\/(?:public\/screenshots|images)\/[^\s"'<>()[\]]+/g)) {
    const path = match[0]
    if (path.includes('..')) errors.push(`README.md: invalid screenshot path ${path}`)
    else files.add(path)
    const screenshot = /^docs\/public\/screenshots\/(.+)-dark\.jpg$/.exec(path)
    if (screenshot && stemPattern.test(screenshot[1])) stems.add(screenshot[1])
  }
  for (const stem of stems) {
    files.add(`docs/public/screenshots/${stem}-dark.jpg`)
  }
  for (const path of files) {
    try {
      const bytes = await readFile(join(repoRoot, path))
      if (/\.jpe?g$/i.test(path)) {
        const size = jpegDimensions(bytes)
        actualDimensions.set(path, size)
        if (path.startsWith('docs/public/screenshots/') && size.width * 10 !== size.height * 16) {
          errors.push(`${path}: screenshots must use a 16:10 aspect ratio (${size.width}x${size.height})`)
        }
      }
    } catch (error) {
      errors.push(`${path}: ${error.code === 'ENOENT' ? 'missing screenshot' : error.message}`)
    }
  }
  for (const declared of declaredDimensions) {
    const actual = actualDimensions.get(declared.path)
    if (actual && (declared.width !== actual.width || declared.height !== actual.height)) {
      errors.push(`${declared.location}: declared ${declared.width}x${declared.height} does not match ${declared.path} (${actual.width}x${actual.height})`)
    }
  }
  return { errors, references, screenshots: files.size }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
  const { errors, references, screenshots } = await checkScreenshots(repoRoot)
  if (errors.length) {
    console.error(`Screenshot validation failed (${errors.length}):\n${errors.map((error) => `- ${error}`).join('\n')}`)
    process.exitCode = 1
  } else {
    console.log(`Screenshot validation passed: ${references} documentation references, ${screenshots} screenshots, README paths checked.`)
  }
}

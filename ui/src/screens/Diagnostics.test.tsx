import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../lib/api'
import type { DiagnosticDescriptor, DiagnosticJob } from '../lib/api'
import { Diagnostics } from './Diagnostics'

const mocks = vi.hoisted(() => ({
  diagnostics: vi.fn(),
  startDiagnostics: vi.fn(),
  diagnostic: vi.fn(),
  cancelDiagnostics: vi.fn(),
  downloadDiagnostics: vi.fn(),
}))

vi.mock('../lib/api', async (importOriginal) => {
  const original = await importOriginal<typeof import('../lib/api')>()
  return { ...original, api: { ...original.api, ...mocks } }
})

const queuedJob: DiagnosticJob = {
  id: 'job-1',
  state: 'collecting',
  phase: 'Collecting stored evidence',
  progress_percent: 35,
  created_at: Date.now() - 2_000,
  started_at: Date.now() - 1_000,
}

function descriptor(jobs: DiagnosticJob[] = []): DiagnosticDescriptor {
  return {
    mode: 'stored',
    router_management_calls: false,
    router_changes: false,
    sections: [
      { id: 'controller', label: 'Controller', description: 'Version and health.' },
      { id: 'devices', label: 'Devices', description: 'Stored router evidence.' },
      { id: 'coverage', label: 'Coverage', description: 'Stored collection coverage.' },
      { id: 'events', label: 'Events', description: 'Bounded event summaries.' },
      { id: 'logs', label: 'Controller logs', description: 'Bounded controller logs.' },
    ],
    excluded_secret_classes: [
      'controller passphrases',
      'password hashes',
      'session and CSRF tokens',
      'router credentials',
      'Wi-Fi keys',
      'private keys and certificates',
      'raw database and keyring',
      'client notes and fixed-address assignments',
    ],
    limits: {
      devices: 100,
      sources: 100,
      events: 5_000,
      controller_log_input_bytes: 2_000_000,
      controller_log_output_bytes: 500_000,
      archive_bytes: 10_000_000,
      history: 10,
      retention_seconds: 86_400,
      collection_timeout_seconds: 30,
    },
    controller_log: { available: true, gaps: [] },
    jobs,
  }
}

describe('Diagnostics', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.diagnostics.mockResolvedValue(descriptor())
    mocks.diagnostic.mockImplementation(() => new Promise(() => {}))
  })

  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('shows the complete stored-only disclosure before generation', async () => {
    render(<Diagnostics />)

    const generate = await screen.findByRole('button', { name: 'Generate stored-only bundle' }) as HTMLButtonElement
    expect(generate.disabled).toBe(false)
    expect(screen.getByText('mode=stored')).toBeTruthy()
    expect(screen.getByText('router_management_calls=false')).toBeTruthy()
    expect(screen.getByText('router_changes=false')).toBeTruthy()
    expect(screen.getByText('Controller')).toBeTruthy()
    expect(screen.getByText('session and CSRF tokens')).toBeTruthy()
  })

  it('fails closed when any disclosure field is missing', async () => {
    const partial = { ...descriptor() } as Partial<DiagnosticDescriptor>
    delete partial.router_changes
    mocks.diagnostics.mockResolvedValue(partial)

    render(<Diagnostics />)

    expect((await screen.findByRole('alert')).textContent).toContain('router_changes=false is missing')
    const generate = screen.getByRole('button', { name: 'Generate stored-only bundle' }) as HTMLButtonElement
    expect(generate.disabled).toBe(true)
    fireEvent.click(generate)
    expect(mocks.startDiagnostics).not.toHaveBeenCalled()
  })

  it.each([
    ['section set', { sections: descriptor().sections.slice(0, -1) }, 'exact included-section'],
    ['secret set', { excluded_secret_classes: ['password hashes'] }, 'exact excluded-secret'],
    ['bounded archive', {
      limits: { ...descriptor().limits, archive_bytes: (17 << 20) + 1 },
    }, 'safety ceiling'],
  ])('fails closed when the exact %s disclosure is absent', async (_name, change, message) => {
    mocks.diagnostics.mockResolvedValue({ ...descriptor(), ...change })

    render(<Diagnostics />)

    expect((await screen.findByRole('alert')).textContent).toContain(message)
    expect((screen.getByRole('button', { name: 'Generate stored-only bundle' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it.each([
    ['unsafe progress', { ...queuedJob, progress_percent: 35.5 }, 'job progress is invalid'],
    ['unsafe timestamp', { ...queuedJob, created_at: Number.MAX_SAFE_INTEGER + 1 }, 'job timestamp is invalid'],
    ['reversed timestamps', { ...queuedJob, started_at: queuedJob.created_at - 1 }, 'timestamps are out of order'],
    ['unfinished completed job', {
      ...queuedJob, state: 'completed', progress_percent: 99,
      finished_at: Date.now(), expires_at: Date.now() + 10_000, size_bytes: 2_048,
    }, 'not at 100%'],
    ['missing completed size', {
      ...queuedJob, state: 'completed', progress_percent: 100,
      finished_at: Date.now(), expires_at: Date.now() + 10_000,
    }, 'size is unavailable'],
    ['missing started timestamp', {
      ...queuedJob, state: 'completed', progress_percent: 100, started_at: undefined,
      finished_at: Date.now(), expires_at: Date.now() + 10_000, size_bytes: 2_048,
    }, 'missing started_at'],
    ['oversized completed job', {
      ...queuedJob, state: 'completed', progress_percent: 100,
      finished_at: Date.now(), expires_at: Date.now() + 10_000, size_bytes: 10_000_001,
    }, 'size_bytes is invalid'],
    ['missing terminal expiry', {
      ...queuedJob, state: 'cancelled', finished_at: Date.now(),
    }, 'missing its expiry'],
  ] as const)('fails closed for %s', async (_name, job, message) => {
    mocks.diagnostics.mockResolvedValue(descriptor([job as DiagnosticJob]))

    render(<Diagnostics />)

    expect((await screen.findByRole('alert')).textContent).toContain(message)
  })

  it('shows durable progress and waits for cancellation to become terminal', async () => {
    mocks.startDiagnostics.mockResolvedValue({ job: queuedJob })
    mocks.cancelDiagnostics.mockResolvedValue({
      job: {
        ...queuedJob, state: 'cancelled', phase: 'Cancelled', progress_percent: 35,
        finished_at: Date.now(), expires_at: Date.now() + 60_000,
      },
    })

    render(<Diagnostics />)
    fireEvent.click(await screen.findByRole('button', { name: 'Generate stored-only bundle' }))

    const progress = await screen.findByRole('progressbar', { name: 'Diagnostics bundle progress' })
    expect(progress.getAttribute('aria-valuenow')).toBe('35')
    expect(progress.getAttribute('aria-valuetext')).toBe('Collecting stored evidence, 35%')
    fireEvent.click(screen.getByRole('button', { name: 'Cancel generation' }))

    await waitFor(() => expect(mocks.cancelDiagnostics).toHaveBeenCalledWith('job-1'))
    expect(await screen.findByText('cancelled')).toBeTruthy()
    expect(screen.queryByRole('progressbar')).toBeNull()
  })

  it('refreshes server truth when another tab wins the start race', async () => {
    mocks.diagnostics
      .mockResolvedValueOnce(descriptor())
      .mockResolvedValueOnce(descriptor([queuedJob]))
    mocks.startDiagnostics.mockRejectedValue(new ApiError(409, 'diagnostic_in_progress'))

    render(<Diagnostics />)
    fireEvent.click(await screen.findByRole('button', { name: 'Generate stored-only bundle' }))

    expect(await screen.findByRole('progressbar')).toBeTruthy()
    expect(mocks.diagnostics).toHaveBeenCalledTimes(2)
  })

  it('downloads only a completed ZIP and uses the server filename', async () => {
    const completed: DiagnosticJob = {
      ...queuedJob,
      id: 'job/complete',
      state: 'completed',
      phase: 'Complete',
      progress_percent: 100,
      finished_at: Date.now() - 1_000,
      expires_at: Date.now() + 60_000,
      size_bytes: 2_048,
    }
    mocks.diagnostics.mockResolvedValue(descriptor([completed]))
    mocks.downloadDiagnostics.mockResolvedValue({
      blob: new Blob(['zip'], { type: 'application/zip' }),
      filename: 'oonfeewrt-diagnostics-job.zip',
    })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const createObjectURL = vi.fn(() => 'blob:diagnostics')
    const revokeObjectURL = vi.fn()
    vi.stubGlobal('URL', { createObjectURL, revokeObjectURL })

    render(<Diagnostics />)
    fireEvent.click(await screen.findByRole('button', { name: 'Download ZIP' }))

    await waitFor(() => expect(mocks.downloadDiagnostics).toHaveBeenCalledWith('job/complete', 10_000_000, 2_048))
    expect(createObjectURL).toHaveBeenCalledOnce()
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:diagnostics')
    expect(click).toHaveBeenCalledOnce()
    expect(document.querySelector('a[download="oonfeewrt-diagnostics-job.zip"]')).toBeNull()
    click.mockRestore()
  })

  it('removes a temporary download link and defers object URL cleanup when clicking fails', async () => {
    const completed: DiagnosticJob = {
      ...queuedJob,
      id: 'job-complete',
      state: 'completed',
      phase: 'Complete',
      progress_percent: 100,
      finished_at: Date.now() - 1_000,
      expires_at: Date.now() + 60_000,
      size_bytes: 3,
    }
    mocks.diagnostics.mockResolvedValue(descriptor([completed]))
    mocks.downloadDiagnostics.mockResolvedValue({
      blob: new Blob(['zip'], { type: 'application/zip' }),
      filename: 'diagnostics.zip',
    })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {
      throw new Error('click failed')
    })
    const createObjectURL = vi.fn(() => 'blob:diagnostics')
    const revokeObjectURL = vi.fn()
    vi.stubGlobal('URL', { createObjectURL, revokeObjectURL })

    render(<Diagnostics />)
    fireEvent.click(await screen.findByRole('button', { name: 'Download ZIP' }))

    await waitFor(() => expect(revokeObjectURL).toHaveBeenCalledWith('blob:diagnostics'))
    expect(document.querySelector('a[download="diagnostics.zip"]')).toBeNull()
    expect(click).toHaveBeenCalledOnce()
  })
})

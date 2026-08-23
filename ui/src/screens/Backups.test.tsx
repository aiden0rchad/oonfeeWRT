import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '../lib/api'
import type {
  BackupDescriptor, BackupJob, RestoreDescriptor, RestorePreview, SessionInfo,
} from '../lib/api'
import { Backups } from './Backups'

vi.mock('../lib/api', async () => {
  const actual = await vi.importActual<typeof import('../lib/api')>('../lib/api')
  return { ...actual, api: {
    ...actual.api,
    backups: vi.fn(),
    backup: vi.fn(),
    startBackup: vi.fn(),
    cancelBackup: vi.fn(),
    backupDownloadURL: vi.fn((id: string) => `/api/v1/backups/${encodeURIComponent(id)}/download`),
    restores: vi.fn(),
    uploadRestore: vi.fn(),
    startRestorePreview: vi.fn(),
    restorePreview: vi.fn(),
    cancelRestorePreview: vi.fn(),
    confirmRestore: vi.fn(),
    restoreSuppression: vi.fn(),
    resumeRouterWrites: vi.fn(),
    reauthenticate: vi.fn(),
  } }
})

const session: SessionInfo = {
  admin_id: 1,
  username: 'owner',
  role: 'owner',
  role_label: 'Owner',
  csrf: 'csrf',
  reauthenticated_until: Math.floor(Date.now() / 1_000) + 300,
}

const descriptor: BackupDescriptor = {
  descriptor: {
    plan_id: 'controller-backup-export-v1',
    format: 'oonfeewrt-portable-backup',
    format_version: 1,
    file_extension: '.oowrtbak',
    snapshot: 'Online, transactionally consistent SQLite snapshot.',
    encryption: 'XChaCha20-Poly1305 with a separately wrapped controller data key.',
    includes: [
      'Controller database and account password hashes.',
      'Saved credentials in controller-encrypted form.',
      'Portable controller data key wrapped by the export passphrase.',
    ],
    excludes: [
      'Controller runtime passphrase and browser sessions.',
      'Router firmware and files.',
      'Other backup and diagnostic artifacts.',
    ],
  },
  disclosure: {
    router_management_calls: false,
    router_changes: false,
    automatic_router_apply: false,
    separate_export_passphrase: true,
    export_passphrase_recoverable: false,
    summary: 'Anyone with the file and passphrase can recover its sensitive controller state.',
  },
  limits: {
    history: 5,
    retention_seconds: 900,
    export_timeout_seconds: 1800,
    min_export_passphrase_characters: 16,
    max_export_passphrase_bytes: 4096,
  },
  jobs: [],
}

const restoreDescriptor: RestoreDescriptor = {
  descriptor: {
    format: 'oonfeewrt-portable-backup',
    format_version: 1,
    upload_content_type: 'application/vnd.oonfeewrt.backup',
    confirmation_contract: 'controller-restore-confirm-v1',
    typed_confirmation: 'RESTORE CONTROLLER',
    confirmation_requires: [
      'Re-enter the export passphrase.',
      'Enter the current destination controller runtime passphrase.',
      'Acknowledge controller restart and active-session revocation.',
      'Acknowledge that router writes stay suppressed after restore until an owner resumes them.',
      'Acknowledge that restored desired configuration is never applied to routers automatically.',
    ],
  },
  disclosure: {
    router_management_calls: false,
    router_changes: false,
    live_controller_changes: false,
    automatic_router_apply: false,
    summary: 'Upload and preview use disposable state and never contact routers.',
  },
  limits: {
    max_upload_bytes: 8_000_000,
    max_database_bytes: 7_000_000,
    history: 5,
    retention_seconds: 1800,
    preview_timeout_seconds: 1800,
    confirmation_timeout_seconds: 1800,
    min_export_passphrase_characters: 16,
    max_export_passphrase_bytes: 4096,
  },
  uploads: [],
  previews: [],
}

function queuedJob(): BackupJob {
  return {
    id: 'job-1',
    state: 'queued',
    phase: 'Waiting to create an online controller snapshot.',
    progress_percent: 0,
    created_at: Date.now(),
  }
}

function completedJob(id = 'download-job'): BackupJob {
  return {
    id,
    state: 'completed',
    phase: 'Encrypted backup ready to download.',
    progress_percent: 100,
    created_at: 1_725_000_000_000,
    started_at: 1_725_000_000_100,
    finished_at: 1_725_000_000_200,
    expires_at: 1_725_000_900_200,
    size_bytes: 4096,
    sha256: 'a'.repeat(64),
    schema_version: 19,
    controller_version: 'v0.1.0',
  }
}

const restoreUpload = {
  id: 'u'.repeat(43),
  created_at: 1_725_000_000_000,
  expires_at: 1_725_001_800_000,
  size_bytes: 4096,
  sha256: 'b'.repeat(64),
}

function queuedRestorePreview(): RestorePreview {
  return {
    id: 'p'.repeat(43),
    upload_id: restoreUpload.id,
    state: 'queued',
    phase: 'waiting',
    progress_percent: 0,
    created_at: 1_725_000_000_100,
  }
}

function completedRestorePreview(): RestorePreview {
  return {
    ...queuedRestorePreview(),
    state: 'completed',
    phase: 'ready',
    progress_percent: 100,
    started_at: 1_725_000_000_200,
    finished_at: 1_725_000_000_300,
    expires_at: 1_725_001_800_300,
    plan_id: `controller-restore-confirm-v1.${'c'.repeat(64)}`,
    manifest: {
      format: 'oonfeewrt-portable-backup',
      format_version: 1,
      created_at: '2026-08-23T12:00:00Z',
      controller_version: 'v0.1.0',
      schema_version: 18,
      database_size_bytes: 2048,
    },
    source_schema: 18,
    target_schema: 19,
    counts: { devices: 2, credentials: 2, owned_sections: 4, wlans: 1, meshes: 0 },
  }
}

function restoreHistory(previews: RestorePreview[] = [], uploads = [restoreUpload]): RestoreDescriptor {
  return { ...structuredClone(restoreDescriptor), uploads, previews }
}

describe('Backups', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(api.backups).mockResolvedValue(structuredClone(descriptor))
    vi.mocked(api.restores).mockResolvedValue(structuredClone(restoreDescriptor))
    vi.mocked(api.restoreSuppression).mockResolvedValue({ suppression: { active: false } })
    vi.mocked(api.startBackup).mockResolvedValue({ job: queuedJob() })
    vi.mocked(api.reauthenticate).mockResolvedValue({
      reauthenticated_until: Math.floor(Date.now() / 1_000) + 300,
    })
  })

  afterEach(() => vi.restoreAllMocks())

  it('keeps sensitive details compact, then sends the exact reviewed plan and fresh acknowledgement', async () => {
    render(<Backups session={session} />)
    expect(await screen.findByText(/Creates an encrypted, transactionally consistent controller backup/)).toBeTruthy()
    expect(screen.queryByLabelText('Export passphrase')).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: 'Review encrypted export' }))
    expect(screen.getByText('plan_id=controller-backup-export-v1')).toBeTruthy()
    fireEvent.change(screen.getByLabelText('Export passphrase'), { target: { value: 'separate export passphrase' } })
    fireEvent.change(screen.getByLabelText('Repeat export passphrase'), { target: { value: 'separate export passphrase' } })
    fireEvent.click(screen.getByRole('checkbox'))
    fireEvent.click(screen.getByRole('button', { name: 'Create encrypted backup' }))

    await waitFor(() => expect(api.startBackup).toHaveBeenCalledWith(
      'controller-backup-export-v1',
      true,
      'separate export passphrase',
      'separate export passphrase',
    ))
    expect(screen.queryByLabelText('Export passphrase')).toBeNull()
    expect(await screen.findByText('Backup in progress')).toBeTruthy()
  })

  it('requires an explicit owner reauthentication and never starts automatically', async () => {
    render(<Backups session={{ ...session, reauthenticated_until: null }} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Review encrypted export' }))
    expect(screen.queryByLabelText('Export passphrase')).toBeNull()
    fireEvent.change(screen.getByLabelText('Current password'), { target: { value: 'current owner password' } })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm identity' }))

    await waitFor(() => expect(api.reauthenticate).toHaveBeenCalledWith('current owner password'))
    expect(api.startBackup).not.toHaveBeenCalled()
    expect((await screen.findByLabelText('Export passphrase') as HTMLInputElement).value).toBe('')
    expect(screen.getByText(/Retry the backup action explicitly/)).toBeTruthy()
  })

  it('rejects an expired reauthentication response and unmounts the owner password field', async () => {
    vi.mocked(api.reauthenticate).mockResolvedValue({
      reauthenticated_until: Math.floor(Date.now() / 1_000) - 1,
    })
    render(<Backups session={{ ...session, reauthenticated_until: null }} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Review encrypted export' }))
    fireEvent.change(screen.getByLabelText('Current password'), { target: { value: 'current owner password' } })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm identity' }))

    expect((await screen.findByRole('alert')).textContent).toMatch(/invalid reauthentication expiry/i)
    expect(screen.queryByLabelText('Current password')).toBeNull()
    expect(screen.queryByLabelText('Export passphrase')).toBeNull()
    expect(api.startBackup).not.toHaveBeenCalled()
  })

  it.each([
    ['mismatched', 'separate export passphrase', 'different export passphrase', /do not match/i],
    ['too short in Unicode code points', '💾'.repeat(8), '💾'.repeat(8), /at least 16 characters/i],
    ['too large in UTF-8 bytes', '💾'.repeat(1_025), '💾'.repeat(1_025), /exceeds 4096 UTF-8 bytes/i],
  ])('clears a %s passphrase pair without sending it', async (_case, passphrase, confirmation, problem) => {
    render(<Backups session={session} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Review encrypted export' }))
    fireEvent.change(screen.getByLabelText('Export passphrase'), { target: { value: passphrase } })
    fireEvent.change(screen.getByLabelText('Repeat export passphrase'), { target: { value: confirmation } })
    fireEvent.click(screen.getByRole('checkbox'))
    fireEvent.submit(screen.getByLabelText('Export passphrase').closest('form')!)

    expect((await screen.findByRole('alert')).textContent).toMatch(problem)
    expect(api.startBackup).not.toHaveBeenCalled()
    expect((screen.getByLabelText('Export passphrase') as HTMLInputElement).value).toBe('')
    expect((screen.getByLabelText('Repeat export passphrase') as HTMLInputElement).value).toBe('')
  })

  it('streams a completed artifact through a native, server-named download instead of a browser Blob', async () => {
    const completed = completedJob()
    vi.mocked(api.backups).mockResolvedValue({ ...structuredClone(descriptor), jobs: [completed] })
    let download = { href: '', target: '', rel: '', filename: '' }
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      download = { href: this.href, target: this.target, rel: this.rel, filename: this.download }
    })
    render(<Backups session={session} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Download .oowrtbak' }))
    expect(download.href).toMatch(/\/api\/v1\/backups\/download-job\/download$/)
    expect(download).toMatchObject({ target: '_blank', rel: 'noopener', filename: '' })
    expect(api.backupDownloadURL).toHaveBeenCalledWith('download-job')
    expect(document.querySelector('a')).toBeNull()
  })

  it('requires a fresh reauthentication before constructing a native download', async () => {
    vi.mocked(api.backups).mockResolvedValue({ ...structuredClone(descriptor), jobs: [completedJob()] })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    render(<Backups session={{ ...session, reauthenticated_until: null }} />)

    fireEvent.click(await screen.findByRole('button', { name: 'Download .oowrtbak' }))

    expect(await screen.findByLabelText('Current password')).toBeTruthy()
    expect(api.backupDownloadURL).not.toHaveBeenCalled()
    expect(click).not.toHaveBeenCalled()
  })

  it('ignores an older status response after a cancellation mutation starts', async () => {
    let resolveRefresh!: (value: BackupDescriptor) => void
    const staleRefresh = new Promise<BackupDescriptor>((resolve) => { resolveRefresh = resolve })
    const active: BackupJob = {
      ...queuedJob(), state: 'snapshotting', phase: 'Creating snapshot.', progress_percent: 10,
      started_at: Date.now(),
    }
    const cancelled: BackupJob = {
      ...active, state: 'cancelled', phase: 'Controller backup export cancelled.',
      finished_at: active.started_at! + 1, expires_at: active.started_at! + 900_001,
    }
    vi.mocked(api.backups)
      .mockResolvedValueOnce({ ...structuredClone(descriptor), jobs: [active] })
      .mockReturnValueOnce(staleRefresh)
    vi.mocked(api.cancelBackup).mockResolvedValue({ job: cancelled })
    render(<Backups session={session} />)
    await screen.findByText('Backup in progress')

    fireEvent.click(screen.getByRole('button', { name: 'Refresh status' }))
    fireEvent.click(screen.getByRole('button', { name: 'Cancel export' }))
    await screen.findByText('cancelled')
    resolveRefresh({ ...structuredClone(descriptor), jobs: [active] })

    await waitFor(() => expect(screen.getByText('cancelled')).toBeTruthy())
    expect(screen.queryByText('Backup in progress')).toBeNull()
  })

  it('fails closed when any safety disclosure is missing', async () => {
    vi.mocked(api.backups).mockResolvedValue({
      ...structuredClone(descriptor),
      disclosure: { ...descriptor.disclosure, router_changes: undefined },
    } as unknown as BackupDescriptor)
    render(<Backups session={session} />)
    expect((await screen.findByRole('alert')).textContent).toMatch(/safety disclosure is incomplete/i)
    expect(screen.queryByRole('button', { name: 'Review encrypted export' })).toBeNull()
  })

  it('clears an entered secret if a refreshed disclosure fails validation', async () => {
    const invalid = {
      ...structuredClone(descriptor),
      disclosure: { ...descriptor.disclosure, router_changes: undefined },
    } as unknown as BackupDescriptor
    vi.mocked(api.backups)
      .mockResolvedValueOnce(structuredClone(descriptor))
      .mockResolvedValueOnce(invalid)
      .mockResolvedValueOnce(structuredClone(descriptor))
    render(<Backups session={session} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Review encrypted export' }))
    fireEvent.change(screen.getByLabelText('Export passphrase'), { target: { value: 'secret draft that must clear' } })
    fireEvent.change(screen.getByLabelText('Repeat export passphrase'), { target: { value: 'secret draft that must clear' } })

    fireEvent.click(screen.getByRole('button', { name: 'Refresh status' }))
    await screen.findByRole('alert')
    fireEvent.click(screen.getByRole('button', { name: 'Refresh disclosure' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Review encrypted export' }))

    expect((screen.getByLabelText('Export passphrase') as HTMLInputElement).value).toBe('')
    expect((screen.getByLabelText('Repeat export passphrase') as HTMLInputElement).value).toBe('')
  })

  it('uploads the native artifact and starts a disposable preview only as separate explicit actions', async () => {
    const file = new File(['encrypted'], 'controller.oowrtbak', {
      type: 'application/vnd.oonfeewrt.backup',
    })
    vi.mocked(api.uploadRestore).mockResolvedValue({ upload: restoreUpload })
    vi.mocked(api.startRestorePreview).mockResolvedValue({ preview: queuedRestorePreview() })
    render(<Backups session={session} />)
    await screen.findByText(/Upload an encrypted .oowrtbak/)

    fireEvent.change(screen.getByLabelText('Encrypted backup file'), { target: { files: [file] } })
    fireEvent.click(screen.getByRole('button', { name: 'Upload encrypted backup' }))
    await waitFor(() => expect(api.uploadRestore).toHaveBeenCalledWith(file))
    expect(api.startRestorePreview).not.toHaveBeenCalled()

    fireEvent.click(await screen.findByRole('button', { name: 'Authenticate and preview' }))
    fireEvent.change(screen.getByLabelText('Backup export passphrase'), {
      target: { value: 'separate restore export passphrase' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Start restore preview' }))

    await waitFor(() => expect(api.startRestorePreview).toHaveBeenCalledWith(
      restoreUpload.id, 'separate restore export passphrase',
    ))
    expect(screen.queryByLabelText('Backup export passphrase')).toBeNull()
    expect(await screen.findByText('Restore preview queued')).toBeTruthy()
  })

  it('fails closed when the restore confirmation descriptor changes', async () => {
    vi.mocked(api.restores).mockResolvedValue({
      ...structuredClone(restoreDescriptor),
      descriptor: { ...restoreDescriptor.descriptor, typed_confirmation: 'RESTORE NOW' },
    } as unknown as RestoreDescriptor)
    render(<Backups session={session} />)

    expect((await screen.findByRole('alert')).textContent).toMatch(/restore confirmation contract changed/i)
    expect((screen.getByRole('button', { name: 'Upload encrypted backup' }) as HTMLButtonElement).disabled).toBe(true)
    expect(api.uploadRestore).not.toHaveBeenCalled()
  })

  it('fails closed when the confirmation timeout disclosure is unavailable', async () => {
    vi.mocked(api.restores).mockResolvedValue({
      ...structuredClone(restoreDescriptor),
      limits: { ...restoreDescriptor.limits, confirmation_timeout_seconds: undefined },
    } as unknown as RestoreDescriptor)
    render(<Backups session={session} />)

    expect((await screen.findByRole('alert')).textContent).toMatch(/restore limits are invalid/i)
    expect(api.uploadRestore).not.toHaveBeenCalled()
  })

  it('fails closed on malformed post-restore suppression state', async () => {
    vi.mocked(api.restoreSuppression).mockResolvedValue({ suppression: {
      active: true,
      restore_id: '0'.repeat(32),
      created_at: '2026-02-30T12:00:00Z',
      reason: 'controller restore completed',
    } })
    render(<Backups session={session} />)

    expect((await screen.findByRole('alert')).textContent).toMatch(/suppression is incomplete/i)
    expect((screen.getByRole('button', { name: 'Upload encrypted backup' }) as HTMLButtonElement).disabled).toBe(true)
    expect(api.resumeRouterWrites).not.toHaveBeenCalled()
  })

  it('unmounts an entered restore passphrase when restore state is refreshed', async () => {
    vi.mocked(api.restores).mockResolvedValue(restoreHistory([], [restoreUpload]))
    render(<Backups session={session} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Authenticate and preview' }))
    fireEvent.change(screen.getByLabelText('Backup export passphrase'), {
      target: { value: 'draft restore passphrase' },
    })

    fireEvent.click(screen.getByRole('button', { name: 'Refresh restore state' }))

    expect(screen.queryByLabelText('Backup export passphrase')).toBeNull()
    await waitFor(() => expect(api.restores).toHaveBeenCalledTimes(2))
    expect(api.startRestorePreview).not.toHaveBeenCalled()
  })

  it('shows authenticated manifest and counts only inside the completed preview disclosure', async () => {
    vi.mocked(api.restores).mockResolvedValue(restoreHistory([completedRestorePreview()]))
    render(<Backups session={session} />)

    expect(await screen.findByText(/v0.1.0 · schema 18 → 19 · 2 devices/)).toBeTruthy()
    const previewNotice = screen.getByRole('group', { name: 'Information: Authenticated restore preview' })
    expect(previewNotice.querySelector('details')?.open).toBe(false)
    expect(screen.getByText('Credentials').closest('div')?.textContent).toContain('2')
    expect(screen.getByText(/Live controller state is still unchanged/)).toBeTruthy()
  })

  it('sends the exact bound confirmation with four distinct acknowledgements then shows reconnect state', async () => {
    const preview = completedRestorePreview()
    vi.mocked(api.restores).mockResolvedValue(restoreHistory([preview]))
    vi.mocked(api.confirmRestore).mockResolvedValue({
      intent: { id: 'd'.repeat(32), state: 'accepted', accepted_at: 1_725_000_000_400 },
    })
    render(<Backups session={session} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Review controller restore' }))
    expect(screen.getByText(/read-only monitoring of restored devices may resume/i)).toBeTruthy()
    expect(screen.getByText(/boot\/keyring secret, not your signed-in account password/i)).toBeTruthy()
    expect(screen.getByLabelText('Destination controller runtime passphrase').getAttribute('autocomplete')).toBe('off')
    fireEvent.change(screen.getByLabelText('Re-enter backup export passphrase'), {
      target: { value: 'separate restore export passphrase' },
    })
    fireEvent.change(screen.getByLabelText('Destination controller runtime passphrase'), {
      target: { value: 'destination runtime passphrase' },
    })
    fireEvent.change(screen.getByLabelText('Type RESTORE CONTROLLER exactly'), {
      target: { value: 'RESTORE CONTROLLER' },
    })
    const acknowledgements = screen.getAllByRole('checkbox')
    expect(acknowledgements).toHaveLength(4)
    acknowledgements.forEach((checkbox) => fireEvent.click(checkbox))
    fireEvent.click(screen.getByRole('button', { name: 'Restore controller' }))

    await waitFor(() => expect(api.confirmRestore).toHaveBeenCalledWith(preview.id, {
      plan_id: preview.plan_id,
      export_passphrase: 'separate restore export passphrase',
      destination_runtime_passphrase: 'destination runtime passphrase',
      typed_confirmation: 'RESTORE CONTROLLER',
      acknowledge_restart: true,
      acknowledge_session_revocation: true,
      acknowledge_router_writes_suppressed: true,
      acknowledge_no_automatic_router_apply: true,
    }))
    expect(screen.queryByLabelText('Re-enter backup export passphrase')).toBeNull()
    expect(await screen.findByText(/controller accepted the bound restore plan and may restart/i)).toBeTruthy()
    expect(screen.getByText(/do not assume router operations resumed/i)).toBeTruthy()
    expect(screen.queryByText(/restore complete/i)).toBeNull()
  })

  it('clears and unmounts confirmation secrets after an error without retrying', async () => {
    vi.mocked(api.restores).mockResolvedValue(restoreHistory([completedRestorePreview()]))
    vi.mocked(api.confirmRestore).mockRejectedValue(new ApiError(422, 'runtime passphrase rejected'))
    render(<Backups session={session} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Review controller restore' }))
    fireEvent.change(screen.getByLabelText('Re-enter backup export passphrase'), {
      target: { value: 'separate restore export passphrase' },
    })
    fireEvent.change(screen.getByLabelText('Destination controller runtime passphrase'), {
      target: { value: 'wrong runtime passphrase' },
    })
    fireEvent.change(screen.getByLabelText('Type RESTORE CONTROLLER exactly'), {
      target: { value: 'RESTORE CONTROLLER' },
    })
    screen.getAllByRole('checkbox').forEach((checkbox) => fireEvent.click(checkbox))
    fireEvent.click(screen.getByRole('button', { name: 'Restore controller' }))

    expect((await screen.findByRole('alert')).textContent).toMatch(/No retry was attempted/i)
    expect(screen.queryByLabelText('Re-enter backup export passphrase')).toBeNull()
    expect(screen.queryByLabelText('Destination controller runtime passphrase')).toBeNull()
    expect(api.confirmRestore).toHaveBeenCalledOnce()
  })

  it('requires recent reauthentication before mounting restore passphrase fields and never resumes automatically', async () => {
    vi.mocked(api.restores).mockResolvedValue(restoreHistory([], [restoreUpload]))
    render(<Backups session={{ ...session, reauthenticated_until: null }} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Authenticate and preview' }))

    expect(screen.queryByLabelText('Backup export passphrase')).toBeNull()
    fireEvent.change(screen.getByLabelText('Current password for restore'), {
      target: { value: 'owner password' },
    })
    fireEvent.click(screen.getAllByRole('button', { name: 'Confirm identity' }).at(-1)!)

    await waitFor(() => expect(api.reauthenticate).toHaveBeenCalledWith('owner password'))
    expect(api.startRestorePreview).not.toHaveBeenCalled()
    expect(screen.queryByLabelText('Backup export passphrase')).toBeNull()
    expect(screen.getByText(/Retry the restore action explicitly/)).toBeTruthy()
  })

  it('discloses automatic neighbour writes and resumes only with the bound id and exact phrase', async () => {
    const restoreID = '1'.repeat(32)
    vi.mocked(api.restoreSuppression).mockResolvedValue({ suppression: {
      active: true,
      restore_id: restoreID,
      created_at: '2026-08-23T12:00:00Z',
      reason: 'controller restore completed',
    } })
    vi.mocked(api.resumeRouterWrites).mockResolvedValue({ suppression: { active: false } })
    render(<Backups session={session} />)

    expect(await screen.findByText(/Router writes are suppressed after a controller restore/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Review router-write resumption' }))
    const resumeNotice = screen.getByRole('group', { name: 'Critical: Resume router writes' })
    expect(resumeNotice.textContent).toMatch(/immediately re-enables automatic 802\.11k neighbour maintenance/i)
    expect(resumeNotice.textContent).toMatch(/may write RRM neighbour state/i)
    fireEvent.change(screen.getByLabelText('Type RESUME ROUTER WRITES exactly'), {
      target: { value: 'RESUME ROUTER WRITES' },
    })
    const resumeButton = screen.getByRole('button', { name: 'Resume router writes' }) as HTMLButtonElement
    expect(resumeButton.disabled).toBe(true)
    fireEvent.click(screen.getByRole('checkbox', {
      name: /I understand that resuming may immediately write 802\.11k RRM neighbour state/i,
    }))
    expect(resumeButton.disabled).toBe(false)
    fireEvent.click(resumeButton)

    await waitFor(() => expect(api.resumeRouterWrites).toHaveBeenCalledWith(restoreID, 'RESUME ROUTER WRITES'))
    expect(await screen.findByText(/No restore-based router-write suppression is active/)).toBeTruthy()
    expect(screen.getByText(/Automatic 802\.11k neighbour maintenance is enabled/i)).toBeTruthy()
    expect(screen.getByText(/No desired-configuration Apply was started/i)).toBeTruthy()
    expect(screen.queryByText(/No router operation was started automatically/)).toBeNull()
  })

  it('blocks confirmation while a prior restore safety gate is active', async () => {
    vi.mocked(api.restores).mockResolvedValue(restoreHistory([completedRestorePreview()]))
    vi.mocked(api.restoreSuppression).mockResolvedValue({ suppression: {
      active: true,
      restore_id: '2'.repeat(32),
      created_at: '2026-08-23T12:00:00Z',
      reason: 'controller restore completed',
    } })
    render(<Backups session={session} />)

    expect(await screen.findByText(/Confirmation is blocked by the prior restore safety gate/)).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Restore blocked by safety gate' }) as HTMLButtonElement).disabled).toBe(true)
    expect(screen.queryByRole('button', { name: 'Review controller restore' })).toBeNull()
    expect(api.confirmRestore).not.toHaveBeenCalled()
  })
})

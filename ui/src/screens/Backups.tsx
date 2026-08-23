import { useCallback, useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { ApiError, api } from '../lib/api'
import type {
  BackupDescriptor,
  BackupJob,
  BackupJobState,
  RestoreConfirmation,
  RestoreDescriptor,
  RestoreIntent,
  RestorePreview,
  RestoreSuppression,
  RestoreUpload,
  SessionInfo,
} from '../lib/api'
import { ago } from '../components/Chart'
import { Banner, Button, Card, Field, Notice } from '../components/ui'

const planID = 'controller-backup-export-v1'
const activeStates = new Set<BackupJobState>(['queued', 'snapshotting', 'encrypting'])
const terminalStates = new Set<BackupJobState>(['completed', 'failed', 'cancelled'])
const jobStates = new Set<BackupJobState>([...activeStates, ...terminalStates])

function record(value: unknown): value is Record<string, unknown> {
  return value != null && typeof value === 'object' && !Array.isArray(value)
}

function jobProblem(value: unknown) {
  if (!record(value) || typeof value.id !== 'string' || !value.id) return 'a job identifier is unavailable'
  if (typeof value.state !== 'string' || !jobStates.has(value.state as BackupJobState)) return 'a job state is invalid'
  if (typeof value.phase !== 'string' || !value.phase) return 'a job phase is unavailable'
  if (!Number.isSafeInteger(value.progress_percent) || (value.progress_percent as number) < 0 ||
    (value.progress_percent as number) > 100) return 'job progress is invalid'
  if (!Number.isSafeInteger(value.created_at) || (value.created_at as number) < 0) return 'a job timestamp is invalid'
  for (const key of ['started_at', 'finished_at', 'expires_at'] as const) {
    if (value[key] != null && (!Number.isSafeInteger(value[key]) || (value[key] as number) < 0)) {
      return `job ${key} is invalid`
    }
  }
  if (value.size_bytes != null && (!Number.isSafeInteger(value.size_bytes) || (value.size_bytes as number) <= 0)) {
    return 'job size is invalid'
  }
  if (value.sha256 != null && (typeof value.sha256 !== 'string' || !/^[0-9a-f]{64}$/.test(value.sha256))) {
    return 'job checksum is invalid'
  }
  if (value.schema_version != null && (!Number.isSafeInteger(value.schema_version) || (value.schema_version as number) <= 0)) {
    return 'job schema version is invalid'
  }
  if (value.controller_version != null && (typeof value.controller_version !== 'string' || !value.controller_version)) {
    return 'job controller version is invalid'
  }
  if (value.error != null && typeof value.error !== 'string') return 'a job error is invalid'

  const state = value.state as BackupJobState
  const created = value.created_at as number
  const started = value.started_at as number | undefined
  const finished = value.finished_at as number | undefined
  const expires = value.expires_at as number | undefined
  if (started != null && started < created || finished != null && finished < (started ?? created) ||
    expires != null && (finished == null || expires <= finished)) return 'job timestamps are out of order'
  if (state !== 'queued' && started == null) return 'a started job is missing started_at'
  if (activeStates.has(state) && (finished != null || expires != null || value.size_bytes != null)) {
    return 'an active job has terminal fields'
  }
  if (terminalStates.has(state) && (finished == null || expires == null)) return 'a terminal job is missing its expiry'
  if (state === 'completed' && (value.progress_percent !== 100 || value.size_bytes == null ||
    value.sha256 == null || value.schema_version == null || value.controller_version == null)) {
    return 'a completed job is missing verified artifact details'
  }
  if (state !== 'completed' && (value.size_bytes != null || value.sha256 != null || value.schema_version != null ||
    value.controller_version != null)) return 'a non-completed job has artifact details'
  return ''
}

function descriptorProblem(value: unknown) {
  if (!record(value) || !record(value.descriptor) || !record(value.disclosure) || !record(value.limits)) {
    return 'the export descriptor is incomplete'
  }
  const descriptor = value.descriptor
  const disclosure = value.disclosure
  const limits = value.limits
  if (descriptor.plan_id !== planID || descriptor.format !== 'oonfeewrt-portable-backup' ||
    descriptor.format_version !== 1 || descriptor.file_extension !== '.oowrtbak' ||
    typeof descriptor.snapshot !== 'string' || !descriptor.snapshot ||
    typeof descriptor.encryption !== 'string' || !descriptor.encryption) return 'the export plan changed'
  for (const key of ['includes', 'excludes'] as const) {
    if (!Array.isArray(descriptor[key]) || descriptor[key].length !== 3 ||
      descriptor[key].some((item) => typeof item !== 'string' || !item)) return `the ${key} disclosure is incomplete`
  }
  if (disclosure.router_management_calls !== false || disclosure.router_changes !== false ||
    disclosure.automatic_router_apply !== false || disclosure.separate_export_passphrase !== true ||
    disclosure.export_passphrase_recoverable !== false || typeof disclosure.summary !== 'string' ||
    !disclosure.summary) return 'the safety disclosure is incomplete'
  if (limits.history !== 5 || limits.min_export_passphrase_characters !== 16 ||
    limits.max_export_passphrase_bytes !== 4096 || !Number.isSafeInteger(limits.retention_seconds) ||
    (limits.retention_seconds as number) <= 0 || !Number.isSafeInteger(limits.export_timeout_seconds) ||
    (limits.export_timeout_seconds as number) <= 0) return 'the export limits are unavailable'
  if (!Array.isArray(value.jobs)) return 'job history is unavailable'
  if (value.jobs.length > limits.history || new Set(value.jobs.map((job) => record(job) ? job.id : undefined)).size !== value.jobs.length) {
    return 'job history is invalid'
  }
  const invalid = value.jobs.map(jobProblem).find(Boolean)
  if (invalid) return invalid
  if (value.jobs.filter((job) => record(job) && activeStates.has(job.state as BackupJobState)).length > 1) {
    return 'more than one active export was returned'
  }
  return ''
}

function passphraseProblem(passphrase: string, confirmation: string, descriptor: BackupDescriptor) {
  if (passphrase.length > descriptor.limits.max_export_passphrase_bytes ||
    confirmation.length > descriptor.limits.max_export_passphrase_bytes) {
    return `The export passphrase exceeds ${descriptor.limits.max_export_passphrase_bytes} UTF-8 bytes. Nothing has run.`
  }
  if (passphrase !== confirmation) return 'The export passphrases do not match. Nothing has run.'
  if ([...passphrase].length < descriptor.limits.min_export_passphrase_characters) {
    return `Use at least ${descriptor.limits.min_export_passphrase_characters} characters. Nothing has run.`
  }
  if (new TextEncoder().encode(passphrase).byteLength > descriptor.limits.max_export_passphrase_bytes) {
    return `The export passphrase exceeds ${descriptor.limits.max_export_passphrase_bytes} UTF-8 bytes. Nothing has run.`
  }
  return ''
}

function errorText(error: unknown) {
  if (error instanceof ApiError && record(error.body) && typeof error.body.error === 'string') return error.body.error
  return error instanceof Error ? error.message : String(error)
}

function formatBytes(value: number | undefined) {
  if (value == null) return 'Size unavailable'
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)} GB`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)} MB`
  if (value >= 1_000) return `${(value / 1_000).toFixed(0)} KB`
  return `${value} B`
}

function mergeJob(current: BackupDescriptor | null, job: BackupJob) {
  if (!current) return null
  return { ...current, jobs: [job, ...current.jobs.filter((item) => item.id !== job.id)]
    .sort((left, right) => right.created_at - left.created_at) }
}

function BackupExport({ session }: { session: SessionInfo }) {
  const [descriptor, setDescriptor] = useState<BackupDescriptor | null>(null)
  const [descriptorIssue, setDescriptorIssue] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState('')
  const [reviewing, setReviewing] = useState(false)
  const [acknowledged, setAcknowledged] = useState(false)
  const [exportPassphrase, setExportPassphrase] = useState('')
  const [confirmPassphrase, setConfirmPassphrase] = useState('')
  const [exportIssue, setExportIssue] = useState('')
  const [reauthPassword, setReauthPassword] = useState('')
  const [reauthenticatedUntil, setReauthenticatedUntil] = useState(session.reauthenticated_until ?? 0)
  const [reauthReason, setReauthReason] = useState('')
  const generation = useRef(0)
  const inFlight = useRef<Promise<BackupDescriptor | null> | null>(null)

  const refresh = useCallback(async () => {
    setExportPassphrase('')
    setConfirmPassphrase('')
    setReauthPassword('')
    setReauthReason('')
    setAcknowledged(false)
    setExportIssue('')
    setReviewing(false)
    if (inFlight.current) return inFlight.current
    const current = ++generation.current
    const request = api.backups().then((next) => {
      if (current !== generation.current) return null
      const problem = descriptorProblem(next)
      if (problem) {
        setDescriptor(null)
        setDescriptorIssue(`Backup export is disabled: ${problem}.`)
        setExportPassphrase('')
        setConfirmPassphrase('')
        setAcknowledged(false)
        setExportIssue('')
        setReviewing(false)
        return null
      }
      setDescriptor(next)
      setDescriptorIssue('')
      setError('')
      return next
    }).catch((cause) => {
      if (current === generation.current) {
        setError(errorText(cause))
        setExportPassphrase('')
        setConfirmPassphrase('')
        setAcknowledged(false)
        setReviewing(false)
      }
      return null
    }).finally(() => {
      if (inFlight.current === request) inFlight.current = null
    })
    inFlight.current = request
    return request
  }, [])

  useEffect(() => {
    void refresh()
    return () => { generation.current++; inFlight.current = null }
  }, [refresh])

  const active = descriptor?.jobs.find((job) => activeStates.has(job.state))
  useEffect(() => {
    if (!active || busy) return
    const timer = window.setInterval(() => void refresh(), 1_000)
    return () => window.clearInterval(timer)
  }, [active?.id, busy, refresh])

  function hasRecentReauthentication() {
    return reauthenticatedUntil > Math.floor(Date.now() / 1_000)
  }

  function clearExportSecrets() {
    setExportPassphrase('')
    setConfirmPassphrase('')
    setAcknowledged(false)
  }

  function requireReauthentication(reason: string) {
    setReauthReason(reason)
    setReauthPassword('')
    setError('Confirm your current password, then retry the action. Nothing has run.')
  }

  async function reauthenticate(event: FormEvent) {
    event.preventDefault()
    setBusy('reauth')
    setError('')
    try {
      const response = await api.reauthenticate(reauthPassword)
      if (!Number.isSafeInteger(response.reauthenticated_until) ||
        response.reauthenticated_until <= Math.floor(Date.now() / 1_000)) {
        throw new Error('The controller returned an invalid reauthentication expiry.')
      }
      setReauthenticatedUntil(response.reauthenticated_until)
      setReauthReason('')
      setNotice('Identity confirmed. Retry the backup action explicitly.')
    } catch (cause) {
      setReauthReason('')
      setError(errorText(cause))
    } finally {
      setReauthPassword('')
      setBusy('')
    }
  }

  async function start(event: FormEvent) {
    event.preventDefault()
    if (!descriptor || descriptorIssue || error || !acknowledged || active || busy) return
    if (!hasRecentReauthentication()) {
      clearExportSecrets()
      requireReauthentication('export')
      return
    }
    const passphrase = exportPassphrase
    const confirmation = confirmPassphrase
    clearExportSecrets()
    const problem = passphraseProblem(passphrase, confirmation, descriptor)
    if (problem) {
      setExportIssue(problem)
      return
    }
    setExportIssue('')
    setReviewing(false)
    setBusy('start')
    setError('')
    setNotice('')
    generation.current++
    inFlight.current = null
    try {
      const { job } = await api.startBackup(descriptor.descriptor.plan_id, true, passphrase, confirmation)
      const problem = jobProblem(job)
      if (problem) throw new Error(`Backup start response is incomplete: ${problem}.`)
      setDescriptor((current) => mergeJob(current, job))
      setNotice('Encrypted controller backup started. The passphrase was cleared from the form.')
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 428) requireReauthentication('export')
      else setError(errorText(cause))
      void refresh()
    } finally {
      setBusy('')
    }
  }

  async function cancel() {
    if (!active || busy) return
    if (!hasRecentReauthentication()) {
      requireReauthentication('cancel')
      return
    }
    generation.current++
    inFlight.current = null
    setBusy('cancel')
    setError('')
    try {
      const { job } = await api.cancelBackup(active.id)
      const problem = jobProblem(job)
      if (problem) throw new Error(`Backup cancel response is incomplete: ${problem}.`)
      setDescriptor((current) => mergeJob(current, job))
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 428) requireReauthentication('cancel')
      else setError(errorText(cause))
      void refresh()
    } finally {
      setBusy('')
    }
  }

  function download(job: BackupJob) {
    if (job.state !== 'completed') return
    if (!hasRecentReauthentication()) {
      requireReauthentication(`download:${job.id}`)
      return
    }
    const link = document.createElement('a')
    link.href = api.backupDownloadURL(job.id)
    link.target = '_blank'
    link.rel = 'noopener'
    document.body.appendChild(link)
    link.click()
    link.remove()
    setNotice('Download requested. The server supplies the verified filename; authorization or expiry errors open separately and are not saved as backups.')
  }

  const history = (descriptor?.jobs ?? []).filter((job) => job.id !== active?.id)
    .slice(0, descriptor?.limits.history ?? 0)
  const progress = active ? Math.max(0, Math.min(100, active.progress_percent)) : 0
  const reauthenticated = hasRecentReauthentication()

  return <div className="diagnostics-page backup-page">
    {descriptorIssue && <div role="alert"><Banner tone="critical">{descriptorIssue} Refresh before exporting.</Banner></div>}
    {error && <div role="alert"><Banner tone="critical">Backup unavailable: {error}</Banner></div>}
    {notice && <div role="status"><Banner tone="accent">{notice}</Banner></div>}

    {reauthReason && !reauthenticated && <Card title="Confirm owner identity">
      <Notice
        component="Owner reauthentication"
        summary="Enter your current controller password. The blocked action will not run automatically."
        details="This password is used only for the reauthentication request and is cleared from the form. Retry the export, cancellation, or download yourself after confirmation."
        defaultOpen
        actions={<form className="account-reauth" onSubmit={reauthenticate}>
          <Field label="Current password" type="password" value={reauthPassword} autoComplete="current-password"
            disabled={busy !== ''} required onChange={(event) => setReauthPassword(event.target.value)} />
          <Button type="submit" kind="primary" disabled={busy !== '' || !reauthPassword}>
            {busy === 'reauth' ? 'Confirming…' : 'Confirm identity'}
          </Button>
        </form>}
      />
    </Card>}

    <Card title="Controller backup export" actions={<span className="diagnostics-mode">Encrypted · no router calls</span>}>
      {!descriptor && !descriptorIssue && !error ? <div role="status">Loading backup disclosure…</div> : descriptor && <>
        <Notice
          tone="accent"
          component="Portable controller backup"
          summary="Creates an encrypted, transactionally consistent controller backup with a separate export passphrase."
          details={<div className="diagnostics-disclosure">
            <dl className="diagnostics-flags">
              <div><dt>Router management calls</dt><dd><code>{String(descriptor.disclosure.router_management_calls)}</code></dd></div>
              <div><dt>Router changes</dt><dd><code>{String(descriptor.disclosure.router_changes)}</code></dd></div>
              <div><dt>Automatic router Apply</dt><dd><code>{String(descriptor.disclosure.automatic_router_apply)}</code></dd></div>
              <div><dt>Format</dt><dd><code>{descriptor.descriptor.format} v{descriptor.descriptor.format_version}</code></dd></div>
            </dl>
            <div><strong>Includes</strong></div>
            <ul>{descriptor.descriptor.includes.map((item) => <li key={item}>{item}</li>)}</ul>
            <div><strong>Excludes</strong></div>
            <ul>{descriptor.descriptor.excludes.map((item) => <li key={item}>{item}</li>)}</ul>
            <div>{descriptor.descriptor.snapshot}</div>
            <div>{descriptor.descriptor.encryption}</div>
            <div>Completed artifacts remain downloadable for {descriptor.limits.retention_seconds} seconds.</div>
          </div>}
          actions={<>
            <Button kind="primary" disabled={busy !== '' || Boolean(active) || Boolean(error)} onClick={() => {
              setReviewing(true)
              setError('')
              setNotice('')
              if (!hasRecentReauthentication()) requireReauthentication('export')
            }}>{active ? 'Export in progress' : reviewing ? 'Export plan open' : 'Review encrypted export'}</Button>
            <Button disabled={busy !== ''} onClick={() => void refresh()}>Refresh status</Button>
          </>}
        />

        {reviewing && <Notice
          component="Backup export review"
          summary="The file contains sensitive controller state. Losing its separate export passphrase makes it unrecoverable."
          details={<div className="diagnostics-disclosure">
            <div>{descriptor.disclosure.summary}</div>
            <div><code>plan_id={descriptor.descriptor.plan_id}</code></div>
            <div><code>file_extension={descriptor.descriptor.file_extension}</code></div>
          </div>}
          defaultOpen
          actions={reauthenticated ? <form className="backup-export-form" onSubmit={start}>
            <Field label="Export passphrase" type="password" value={exportPassphrase} autoComplete="new-password"
              minLength={descriptor.limits.min_export_passphrase_characters}
              maxLength={descriptor.limits.max_export_passphrase_bytes}
              disabled={busy !== ''} required onChange={(event) => { setExportIssue(''); setExportPassphrase(event.target.value) }} />
            <Field label="Repeat export passphrase" type="password" value={confirmPassphrase} autoComplete="new-password"
              minLength={descriptor.limits.min_export_passphrase_characters}
              maxLength={descriptor.limits.max_export_passphrase_bytes}
              disabled={busy !== ''} required onChange={(event) => { setExportIssue(''); setConfirmPassphrase(event.target.value) }} />
            {exportIssue && <div className="account-inline-error" role="alert">{exportIssue}</div>}
            <label className="checkline"><input type="checkbox" checked={acknowledged} disabled={busy !== ''}
              onChange={(event) => setAcknowledged(event.target.checked)} />
              <span>I understand this file includes account password hashes, controller settings, and encrypted saved credentials, and that its export passphrase cannot be recovered.</span>
            </label>
            <div className="notice-actions">
              <Button type="submit" kind="primary" disabled={busy !== '' || Boolean(error) || Boolean(descriptorIssue) || !acknowledged ||
                !exportPassphrase || !confirmPassphrase}>
                {busy === 'start' ? 'Starting…' : 'Create encrypted backup'}
              </Button>
              <Button type="button" disabled={busy !== ''} onClick={() => {
                clearExportSecrets(); setExportIssue(''); setReviewing(false)
              }}>Cancel review</Button>
            </div>
          </form> : <div>Confirm your owner identity above before entering the export passphrase.</div>}
        />}

        {active && <div className="diagnostics-active" aria-live="polite">
          <div className="diagnostics-active-heading"><div><strong>Backup in progress</strong><div>{active.phase}</div></div>
            <Button disabled={busy !== ''} onClick={() => void cancel()}>{busy === 'cancel' ? 'Cancelling…' : 'Cancel export'}</Button>
          </div>
          <div className="diagnostics-progress" role="progressbar" aria-label="Controller backup export progress"
            aria-valuemin={0} aria-valuemax={100} aria-valuenow={progress} aria-valuetext={`${active.phase}, ${progress}%`}>
            <span style={{ width: `${progress}%` }} />
          </div>
        </div>}
      </>}
      {!descriptor && (descriptorIssue || error) && <div className="diagnostics-disabled-action">
        <Button kind="primary" disabled>Create encrypted backup</Button>
        <Button onClick={() => void refresh()}>Refresh disclosure</Button>
      </div>}
    </Card>

    <Card title="Recent encrypted backups" pad={false}>
      {history.length === 0 ? <div className="diagnostics-empty">{descriptor ? 'No completed, failed or cancelled exports yet.' : 'History unavailable.'}</div>
        : <div className="diagnostics-history">{history.map((job) => <div className="diagnostics-history-row" key={job.id}>
          <div><div className="diagnostics-job-title"><strong>{job.state}</strong><span>{job.phase}</span></div>
            <div className="diagnostics-job-detail">
              {ago(Math.floor((job.finished_at ?? job.created_at) / 1_000))} · {formatBytes(job.size_bytes)}
              {job.schema_version ? ` · schema ${job.schema_version}` : ''}
              {job.controller_version ? ` · ${job.controller_version}` : ''}
              {job.expires_at ? ` · expires ${new Date(job.expires_at).toLocaleString()}` : ''}
              {job.error ? ` · ${job.error}` : ''}
            </div>
          </div>
          {job.state === 'completed' && <Button disabled={busy !== ''} onClick={() => download(job)}>
            Download .oowrtbak
          </Button>}
        </div>)}</div>}
    </Card>
  </div>
}

const restoreContentType = 'application/vnd.oonfeewrt.backup'
const restorePlanContract = 'controller-restore-confirm-v1'
const restoreConfirmation = 'RESTORE CONTROLLER'
const resumeConfirmation = 'RESUME ROUTER WRITES'
const restoreRequirements = [
  'Re-enter the export passphrase.',
  'Enter the current destination controller runtime passphrase.',
  'Acknowledge controller restart and active-session revocation.',
  'Acknowledge that router writes stay suppressed after restore until an owner resumes them.',
  'Acknowledge that restored desired configuration is never applied to routers automatically.',
]
const restoreActiveStates = new Set(['queued', 'running'])
const restoreTerminalStates = new Set(['completed', 'failed', 'cancelled'])
const restoreStates = new Set([...restoreActiveStates, ...restoreTerminalStates])
const restoreArtifactIDPattern = /^[A-Za-z0-9_-]{43}$/
const restoreIntentIDPattern = /^(?!0{32}$)[0-9a-f]{32}$/
const restorePlanPattern = /^controller-restore-confirm-v1\.[0-9a-f]{64}$/

function positiveInteger(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) > 0
}

function timestamp(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0
}

function utcTimestamp(value: unknown): value is string {
  if (typeof value !== 'string' || value.length > 64 ||
    !/^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d{1,9})?Z$/.test(value)) return false
  const parsed = new Date(value)
  return Number.isFinite(parsed.valueOf()) && parsed.toISOString().slice(0, 19) === value.slice(0, 19)
}

function restoreUploadProblem(value: unknown, maxBytes: number) {
  if (!record(value) || typeof value.id !== 'string' || !restoreArtifactIDPattern.test(value.id)) {
    return 'an upload identifier is invalid'
  }
  if (!timestamp(value.created_at) || !timestamp(value.expires_at) || value.expires_at <= value.created_at) {
    return 'upload timestamps are invalid'
  }
  if (!positiveInteger(value.size_bytes) || value.size_bytes > maxBytes) return 'an upload size is invalid'
  if (typeof value.sha256 !== 'string' || !/^[0-9a-f]{64}$/.test(value.sha256)) {
    return 'an upload checksum is invalid'
  }
  return ''
}

function restoreManifestProblem(value: unknown, maxDatabaseBytes: number) {
  if (!record(value) || value.format !== 'oonfeewrt-portable-backup' || value.format_version !== 1) {
    return 'the authenticated manifest format is invalid'
  }
  if (!utcTimestamp(value.created_at)) return 'the authenticated manifest time is invalid'
  if (typeof value.controller_version !== 'string' || !value.controller_version ||
    value.controller_version.length > 128) return 'the authenticated controller version is invalid'
  if (!positiveInteger(value.schema_version) || !positiveInteger(value.database_size_bytes) ||
    value.database_size_bytes > maxDatabaseBytes) return 'the authenticated manifest bounds are invalid'
  return ''
}

function restoreCountsProblem(value: unknown) {
  if (!record(value)) return 'restore counts are unavailable'
  for (const key of ['devices', 'credentials', 'owned_sections', 'wlans', 'meshes']) {
    if (!Number.isSafeInteger(value[key]) || (value[key] as number) < 0) return `restore ${key} count is invalid`
  }
  return ''
}

function restorePreviewProblem(value: unknown, limits: RestoreDescriptor['limits'], uploadID?: string) {
  if (!record(value) || typeof value.id !== 'string' || !restoreArtifactIDPattern.test(value.id) ||
    typeof value.upload_id !== 'string' || !restoreArtifactIDPattern.test(value.upload_id) ||
    (uploadID != null && value.upload_id !== uploadID)) return 'a restore preview identity is invalid'
  if (typeof value.state !== 'string' || !restoreStates.has(value.state)) return 'a restore preview state is invalid'
  if (typeof value.phase !== 'string' || !value.phase || !Number.isSafeInteger(value.progress_percent) ||
    (value.progress_percent as number) < 0 || (value.progress_percent as number) > 100) {
    return 'restore preview progress is invalid'
  }
  if (!timestamp(value.created_at)) return 'a restore preview timestamp is invalid'
  for (const key of ['started_at', 'finished_at', 'expires_at']) {
    if (value[key] != null && !timestamp(value[key])) return `restore preview ${key} is invalid`
  }
  const state = value.state as string
  const started = value.started_at as number | undefined
  const finished = value.finished_at as number | undefined
  const expires = value.expires_at as number | undefined
  if (started != null && started < (value.created_at as number) ||
    finished != null && finished < (started ?? value.created_at as number) ||
    expires != null && (finished == null || expires <= finished)) return 'restore preview timestamps are out of order'
  if (state === 'queued' && (started != null || finished != null || value.progress_percent !== 0)) {
    return 'a queued restore preview has started fields'
  }
  if (state === 'running' && (started == null || finished != null || expires != null)) {
    return 'a running restore preview has invalid lifecycle fields'
  }
  if (restoreTerminalStates.has(state) && (finished == null || expires == null || value.progress_percent !== 100)) {
    return 'a terminal restore preview is incomplete'
  }
  if (state === 'completed') {
    if (typeof value.plan_id !== 'string' || !restorePlanPattern.test(value.plan_id) ||
      !positiveInteger(value.source_schema) || !positiveInteger(value.target_schema) ||
      (value.target_schema as number) < (value.source_schema as number)) return 'the restore confirmation binding is invalid'
    const manifestProblem = restoreManifestProblem(value.manifest, limits.max_database_bytes)
    if (manifestProblem) return manifestProblem
    if ((value.manifest as Record<string, unknown>).schema_version !== value.source_schema) {
      return 'the manifest and source schema do not match'
    }
    const countsProblem = restoreCountsProblem(value.counts)
    if (countsProblem) return countsProblem
    if (value.error != null || value.error_code != null) return 'a completed restore preview includes an error'
  } else if (value.plan_id != null || value.manifest != null || value.source_schema != null ||
    value.target_schema != null || value.counts != null) {
    return 'an incomplete restore preview published confirmation facts'
  }
  if (state === 'failed' && (typeof value.error !== 'string' || !value.error ||
    typeof value.error_code !== 'string' || !value.error_code)) return 'a failed restore preview has no safe error'
  if (state !== 'failed' && (value.error != null || value.error_code != null)) {
    return 'a non-failed restore preview includes an error'
  }
  return ''
}

function restoreDescriptorProblem(value: unknown) {
  if (!record(value) || !record(value.descriptor) || !record(value.disclosure) ||
    !record(value.limits) || !Array.isArray(value.uploads) || !Array.isArray(value.previews)) {
    return 'the restore descriptor is incomplete'
  }
  const descriptor = value.descriptor
  const disclosure = value.disclosure
  const limits = value.limits
  if (descriptor.format !== 'oonfeewrt-portable-backup' || descriptor.format_version !== 1 ||
    descriptor.upload_content_type !== restoreContentType ||
    descriptor.confirmation_contract !== restorePlanContract ||
    descriptor.typed_confirmation !== restoreConfirmation ||
    !Array.isArray(descriptor.confirmation_requires) ||
    descriptor.confirmation_requires.length !== restoreRequirements.length ||
    descriptor.confirmation_requires.some((item, index) => item !== restoreRequirements[index])) {
    return 'the restore confirmation contract changed'
  }
  if (disclosure.router_management_calls !== false || disclosure.router_changes !== false ||
    disclosure.live_controller_changes !== false || disclosure.automatic_router_apply !== false ||
    typeof disclosure.summary !== 'string' || !disclosure.summary) return 'the restore safety disclosure is incomplete'
  if (!positiveInteger(limits.max_upload_bytes) || !positiveInteger(limits.max_database_bytes) ||
    limits.max_database_bytes > limits.max_upload_bytes || limits.history !== 5 ||
    !positiveInteger(limits.retention_seconds) || !positiveInteger(limits.preview_timeout_seconds) ||
    !positiveInteger(limits.confirmation_timeout_seconds) ||
    limits.min_export_passphrase_characters !== 16 || limits.max_export_passphrase_bytes !== 4096) {
    return 'the restore limits are invalid'
  }
  if (value.uploads.length > limits.history || value.previews.length > limits.history) {
    return 'restore history exceeds its disclosed bound'
  }
  if (new Set(value.uploads.map((upload) => record(upload) ? upload.id : undefined)).size !== value.uploads.length ||
    new Set(value.previews.map((preview) => record(preview) ? preview.id : undefined)).size !== value.previews.length) {
    return 'restore history identities are invalid'
  }
  for (const upload of value.uploads) {
    const problem = restoreUploadProblem(upload, limits.max_upload_bytes as number)
    if (problem) return problem
  }
  const uploadIDs = new Set(value.uploads.map((upload) => (upload as Record<string, unknown>).id))
  for (const preview of value.previews) {
    const problem = restorePreviewProblem(preview, limits as unknown as RestoreDescriptor['limits'])
    if (problem) return problem
    if (!uploadIDs.has((preview as Record<string, unknown>).upload_id)) return 'a restore preview has no retained upload'
  }
  if (value.previews.filter((preview) => record(preview) &&
    restoreActiveStates.has(String(preview.state))).length > 1) return 'more than one restore preview is active'
  return ''
}

function restorePassphraseProblem(passphrase: string, descriptor: RestoreDescriptor, runtime = false) {
  const bytes = new TextEncoder().encode(passphrase).byteLength
  if (bytes > descriptor.limits.max_export_passphrase_bytes) {
    return `The passphrase exceeds ${descriptor.limits.max_export_passphrase_bytes} UTF-8 bytes. Nothing has run.`
  }
  if (!runtime && [...passphrase].length < descriptor.limits.min_export_passphrase_characters) {
    return `Use at least ${descriptor.limits.min_export_passphrase_characters} characters. Nothing has run.`
  }
  if (runtime && !passphrase) return 'Enter the destination controller runtime passphrase. Nothing has run.'
  return ''
}

function restoreIntentProblem(value: unknown) {
  if (!record(value) || typeof value.id !== 'string' || !restoreIntentIDPattern.test(value.id) ||
    value.state !== 'accepted' || !positiveInteger(value.accepted_at)) return 'the accepted restore response is invalid'
  return ''
}

function restoreSuppressionProblem(value: unknown) {
  if (!record(value) || typeof value.active !== 'boolean') return 'router-write suppression status is invalid'
  if (!value.active) {
    if (value.restore_id != null || value.created_at != null || value.reason != null) {
      return 'inactive router-write suppression includes active fields'
    }
    return ''
  }
  if (typeof value.restore_id !== 'string' || !restoreIntentIDPattern.test(value.restore_id) ||
    !utcTimestamp(value.created_at) || typeof value.reason !== 'string' ||
    !value.reason || value.reason.length > 256) return 'active router-write suppression is incomplete'
  return ''
}

function ReconnectAfterRestore({ intent }: { intent: RestoreIntent }) {
  return <div className="diagnostics-page restore-reconnect" aria-live="assertive">
    <Card title="Reconnect to the controller">
      <Notice
        tone="accent"
        component="Controller restore accepted"
        summary="The controller accepted the bound restore plan and may restart. Reconnect and sign in again."
        details={<div className="diagnostics-disclosure">
          <div><code>intent={intent.id}</code></div>
          <div>This page cannot verify completion after the controller process restarts.</div>
          <div>No router configuration was applied by this response. Check router-write suppression after reconnect; do not assume router operations resumed.</div>
          <div>Read-only monitoring of restored devices may resume after restart using restored credentials.</div>
        </div>}
        defaultOpen
        actions={<Button kind="primary" onClick={() => window.location.reload()}>Try reconnecting</Button>}
      />
    </Card>
  </div>
}

export function Backups({ session }: { session: SessionInfo }) {
  const [accepted, setAccepted] = useState<RestoreIntent | null>(null)
  if (accepted) return <ReconnectAfterRestore intent={accepted} />
  return <div className="backup-restore-page">
    <BackupExport session={session} />
    <ControllerRestore session={session} onAccepted={setAccepted} />
  </div>
}

function ControllerRestore({ session, onAccepted }: {
  session: SessionInfo
  onAccepted: (intent: RestoreIntent) => void
}) {
  const [descriptor, setDescriptor] = useState<RestoreDescriptor | null>(null)
  const [suppression, setSuppression] = useState<RestoreSuppression | null>(null)
  const [descriptorIssue, setDescriptorIssue] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState('load')
  const [upload, setUpload] = useState<RestoreUpload | null>(null)
  const [preview, setPreview] = useState<RestorePreview | null>(null)
  const [artifact, setArtifact] = useState<File | null>(null)
  const [previewOpen, setPreviewOpen] = useState(false)
  const [previewPassphrase, setPreviewPassphrase] = useState('')
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [confirmExportPassphrase, setConfirmExportPassphrase] = useState('')
  const [runtimePassphrase, setRuntimePassphrase] = useState('')
  const [typedConfirmation, setTypedConfirmation] = useState('')
  const [resumeOpen, setResumeOpen] = useState(false)
  const [resumeTypedConfirmation, setResumeTypedConfirmation] = useState('')
  const [resumeNeighbourAcknowledged, setResumeNeighbourAcknowledged] = useState(false)
  const [acknowledgements, setAcknowledgements] = useState([false, false, false, false])
  const [formIssue, setFormIssue] = useState('')
  const [reauthPassword, setReauthPassword] = useState('')
  const [reauthReason, setReauthReason] = useState('')
  const [reauthenticatedUntil, setReauthenticatedUntil] = useState(session.reauthenticated_until ?? 0)
  const [pollStopped, setPollStopped] = useState(false)
  const artifactInput = useRef<HTMLInputElement>(null)
  const generation = useRef(0)
  const polling = useRef(false)

  const clearSecrets = useCallback(() => {
    setArtifact(null)
    if (artifactInput.current) artifactInput.current.value = ''
    setPreviewPassphrase('')
    setPreviewOpen(false)
    setConfirmExportPassphrase('')
    setRuntimePassphrase('')
    setTypedConfirmation('')
    setResumeTypedConfirmation('')
    setResumeNeighbourAcknowledged(false)
    setResumeOpen(false)
    setAcknowledgements([false, false, false, false])
    setConfirmOpen(false)
    setReauthPassword('')
    setReauthReason('')
    setFormIssue('')
  }, [])

  function recentlyReauthenticated() {
    return reauthenticatedUntil > Math.floor(Date.now() / 1_000)
  }

  function requireReauthentication(reason: string) {
    clearSecrets()
    setReauthPassword('')
    setReauthReason(reason)
    setError('Confirm your current password, then retry the restore action explicitly. Nothing has run.')
  }

  const load = useCallback(async () => {
    clearSecrets()
    setBusy('load')
    setDescriptorIssue('')
    setError('')
    setNotice('')
    setPollStopped(true)
    const current = ++generation.current
    try {
      const [next, suppressionResponse] = await Promise.all([
        api.restores(), api.restoreSuppression(),
      ])
      if (current !== generation.current) return
      const problem = restoreDescriptorProblem(next)
      const suppressionProblem = restoreSuppressionProblem(suppressionResponse.suppression)
      if (problem || suppressionProblem) {
        setDescriptor(null)
        setSuppression(suppressionProblem ? null : suppressionResponse.suppression)
        setDescriptorIssue(`Controller restore is disabled: ${problem || suppressionProblem}.`)
        setUpload(null)
        setPreview(null)
        return
      }
      const active = next.previews.find((item) => restoreActiveStates.has(item.state))
      const selectedPreview = active ?? next.previews[0] ?? null
      setDescriptor(next)
      setSuppression(suppressionResponse.suppression)
      setDescriptorIssue('')
      setPreview(selectedPreview)
      setUpload(selectedPreview
        ? next.uploads.find((item) => item.id === selectedPreview.upload_id) ?? null
        : next.uploads[0] ?? null)
      setPollStopped(false)
    } catch (cause) {
      if (current === generation.current) {
        clearSecrets()
        setDescriptor(null)
        setSuppression(null)
        setError(errorText(cause))
      }
    } finally {
      if (current === generation.current) setBusy('')
    }
  }, [clearSecrets])

  useEffect(() => {
    void load()
    return () => {
      generation.current++
      polling.current = false
    }
  }, [load])

  const poll = useCallback(async (explicit = false) => {
    if (!descriptor || !preview || !restoreActiveStates.has(preview.state) || polling.current) return
    polling.current = true
    const current = ++generation.current
    if (explicit) {
      setError('')
      setPollStopped(false)
    }
    try {
      const response = await api.restorePreview(preview.id)
      if (current !== generation.current) return
      const problem = restorePreviewProblem(response.preview, descriptor.limits, preview.upload_id)
      if (problem) throw new Error(`Restore preview response is unsafe: ${problem}.`)
      setPreview(response.preview)
      setPollStopped(false)
    } catch (cause) {
      if (current === generation.current) {
        clearSecrets()
        setError(errorText(cause))
        setPollStopped(true)
      }
    } finally {
      polling.current = false
    }
  }, [clearSecrets, descriptor, preview])

  useEffect(() => {
    if (!preview || !restoreActiveStates.has(preview.state) || pollStopped || busy) return
    const timer = window.setInterval(() => void poll(), 1_000)
    return () => window.clearInterval(timer)
  }, [busy, poll, pollStopped, preview])

  async function reauthenticate(event: FormEvent) {
    event.preventDefault()
    setBusy('reauth')
    setError('')
    try {
      const response = await api.reauthenticate(reauthPassword)
      if (!timestamp(response.reauthenticated_until) ||
        response.reauthenticated_until <= Math.floor(Date.now() / 1_000)) {
        throw new Error('The controller returned an invalid reauthentication expiry.')
      }
      setReauthenticatedUntil(response.reauthenticated_until)
      setReauthReason('')
      setNotice('Identity confirmed. Retry the restore action explicitly; nothing ran automatically.')
    } catch (cause) {
      clearSecrets()
      setError(errorText(cause))
    } finally {
      setReauthPassword('')
      setBusy('')
    }
  }

  async function uploadArtifact(event: FormEvent) {
    event.preventDefault()
    if (!descriptor || !artifact || busy) return
    if (!recentlyReauthenticated()) {
      requireReauthentication('upload')
      return
    }
    const file = artifact
    clearSecrets()
    if (!file.name.endsWith('.oowrtbak') || file.size <= 0 || file.size > descriptor.limits.max_upload_bytes) {
      setFormIssue(`Choose a non-empty .oowrtbak file no larger than ${formatBytes(descriptor.limits.max_upload_bytes)}.`)
      return
    }
    setBusy('upload')
    setError('')
    setNotice('')
    const current = ++generation.current
    try {
      const response = await api.uploadRestore(file)
      if (current !== generation.current) return
      const problem = restoreUploadProblem(response.upload, descriptor.limits.max_upload_bytes)
      if (problem) throw new Error(`Restore upload response is unsafe: ${problem}.`)
      setUpload(response.upload)
      setPreview(null)
      setNotice('Encrypted backup uploaded. Nothing was decrypted and no controller or router state changed.')
    } catch (cause) {
      if (current === generation.current) {
        clearSecrets()
        setError(errorText(cause))
      }
    } finally {
      if (current === generation.current) setBusy('')
    }
  }

  async function startPreview(event: FormEvent) {
    event.preventDefault()
    if (!descriptor || !upload || !previewOpen || busy) return
    if (!recentlyReauthenticated()) {
      requireReauthentication('preview')
      return
    }
    const passphrase = previewPassphrase
    clearSecrets()
    const problem = restorePassphraseProblem(passphrase, descriptor)
    if (problem) {
      setFormIssue(problem)
      return
    }
    setBusy('preview')
    setError('')
    setNotice('')
    setPollStopped(false)
    const current = ++generation.current
    try {
      const response = await api.startRestorePreview(upload.id, passphrase)
      if (current !== generation.current) return
      const responseProblem = restorePreviewProblem(response.preview, descriptor.limits, upload.id)
      if (responseProblem) throw new Error(`Restore preview response is unsafe: ${responseProblem}.`)
      setPreview(response.preview)
      setNotice('Restore preview started. Its export passphrase was cleared; no live state changed.')
    } catch (cause) {
      if (current === generation.current) {
        clearSecrets()
        setPollStopped(true)
        if (cause instanceof ApiError && cause.status === 428) requireReauthentication('preview')
        else setError(errorText(cause))
      }
    } finally {
      if (current === generation.current) setBusy('')
    }
  }

  async function cancelPreview() {
    if (!descriptor || !preview || !restoreActiveStates.has(preview.state) || busy) return
    if (!recentlyReauthenticated()) {
      requireReauthentication('cancel')
      return
    }
    clearSecrets()
    setBusy('cancel')
    setError('')
    setPollStopped(true)
    const current = ++generation.current
    try {
      const response = await api.cancelRestorePreview(preview.id)
      if (current !== generation.current) return
      const problem = restorePreviewProblem(response.preview, descriptor.limits, preview.upload_id)
      if (problem) throw new Error(`Restore cancellation response is unsafe: ${problem}.`)
      setPreview(response.preview)
      setNotice('Restore preview cancellation requested. No live controller or router state changed.')
    } catch (cause) {
      if (current === generation.current) {
        clearSecrets()
        if (cause instanceof ApiError && cause.status === 428) requireReauthentication('cancel')
        else setError(errorText(cause))
      }
    } finally {
      if (current === generation.current) setBusy('')
    }
  }

  function openConfirmation() {
    clearSecrets()
    if (suppression?.active) {
      setFormIssue('Controller restore is blocked while a prior restore safety gate is active. Resume router writes, or keep the safety gate and do not restore.')
      return
    }
    if (!recentlyReauthenticated()) {
      requireReauthentication('confirm')
      return
    }
    setError('')
    setNotice('')
    setConfirmOpen(true)
  }

  async function confirm(event: FormEvent) {
    event.preventDefault()
    if (!descriptor || !preview || preview.state !== 'completed' || !preview.plan_id || !confirmOpen || busy) return
    const exportPassphrase = confirmExportPassphrase
    const destinationRuntimePassphrase = runtimePassphrase
    const typed = typedConfirmation
    const acknowledged = acknowledgements.every(Boolean)
    clearSecrets()
    if (suppression?.active) {
      setFormIssue('Controller restore is blocked while a prior restore safety gate is active. Router writes remain suppressed.')
      return
    }
    if (!recentlyReauthenticated()) {
      requireReauthentication('confirm')
      return
    }
    const passphraseIssue = restorePassphraseProblem(exportPassphrase, descriptor) ||
      restorePassphraseProblem(destinationRuntimePassphrase, descriptor, true)
    if (passphraseIssue || typed !== restoreConfirmation || !acknowledged) {
      setFormIssue(passphraseIssue || `Type ${restoreConfirmation} exactly and acknowledge all four consequences. Nothing has run.`)
      return
    }
    const request: RestoreConfirmation = {
      plan_id: preview.plan_id,
      export_passphrase: exportPassphrase,
      destination_runtime_passphrase: destinationRuntimePassphrase,
      typed_confirmation: restoreConfirmation,
      acknowledge_restart: true,
      acknowledge_session_revocation: true,
      acknowledge_router_writes_suppressed: true,
      acknowledge_no_automatic_router_apply: true,
    }
    setBusy('confirm')
    setError('')
    setNotice('')
    setPollStopped(true)
    const current = ++generation.current
    try {
      const response = await api.confirmRestore(preview.id, request)
      if (current !== generation.current) return
      const problem = restoreIntentProblem(response.intent)
      if (problem) throw new Error(`Restore acceptance response is unsafe: ${problem}.`)
      onAccepted(response.intent)
    } catch (cause) {
      if (current === generation.current) {
        clearSecrets()
        if (cause instanceof ApiError && cause.status === 428) requireReauthentication('confirm')
        else setError(`${errorText(cause)} No retry was attempted; review current controller state before submitting again.`)
      }
    } finally {
      if (current === generation.current) setBusy('')
    }
  }

  function openResume() {
    clearSecrets()
    if (!recentlyReauthenticated()) {
      requireReauthentication('resume')
      return
    }
    setError('')
    setNotice('')
    setResumeOpen(true)
  }

  async function resumeRouterWrites(event: FormEvent) {
    event.preventDefault()
    if (!suppression?.active || !resumeOpen || busy) return
    const restoreID = suppression.restore_id
    const typed = resumeTypedConfirmation
    const neighbourAcknowledged = resumeNeighbourAcknowledged
    clearSecrets()
    if (!recentlyReauthenticated()) {
      requireReauthentication('resume')
      return
    }
    if (typed !== resumeConfirmation || !neighbourAcknowledged) {
      setFormIssue(`Type ${resumeConfirmation} exactly and acknowledge automatic 802.11k neighbour writes. Router writes remain suppressed.`)
      return
    }
    setBusy('resume')
    setError('')
    setNotice('')
    const current = ++generation.current
    try {
      const response = await api.resumeRouterWrites(restoreID, resumeConfirmation)
      if (current !== generation.current) return
      const problem = restoreSuppressionProblem(response.suppression)
      if (problem || response.suppression.active) {
        throw new Error(`Router-write resumption response is unsafe: ${problem || 'suppression is still active'}.`)
      }
      setSuppression(response.suppression)
      setNotice('Router writes resumed after explicit owner confirmation. Automatic 802.11k neighbour maintenance is enabled and may write hostapd RRM neighbour state. No desired-configuration Apply was started.')
    } catch (cause) {
      if (current === generation.current) {
        clearSecrets()
        if (cause instanceof ApiError && cause.status === 428) requireReauthentication('resume')
        else setError(`${errorText(cause)} No retry was attempted; refresh router-write status before submitting again.`)
      }
    } finally {
      if (current === generation.current) setBusy('')
    }
  }

  const active = preview != null && restoreActiveStates.has(preview.state)
  const completed = preview?.state === 'completed' ? preview : null
  const reauthenticated = recentlyReauthenticated()

  return <div className="restore-section">
    {descriptorIssue && <div role="alert"><Banner tone="critical">{descriptorIssue} Refresh before uploading.</Banner></div>}
    {error && <div role="alert"><Banner tone="critical">Controller restore unavailable: {error}</Banner></div>}
    {notice && <div role="status"><Banner tone="accent">{notice}</Banner></div>}
    {formIssue && <div role="alert"><Banner tone="critical">{formIssue}</Banner></div>}

    {reauthReason && !reauthenticated && <Card title="Confirm owner identity for restore">
      <Notice
        component="Owner reauthentication"
        summary="Enter your current controller account password. The blocked restore action will not run automatically."
        details="The account password is sent only to the reauthentication endpoint and cleared from this form. Retry the intended action yourself after confirmation."
        defaultOpen
        actions={<form className="account-reauth" onSubmit={reauthenticate}>
          <Field label="Current password for restore" type="password" value={reauthPassword}
            autoComplete="current-password" disabled={busy !== ''} required
            onChange={(event) => setReauthPassword(event.target.value)} />
          <Button type="submit" kind="primary" disabled={busy !== '' || !reauthPassword}>
            {busy === 'reauth' ? 'Confirming…' : 'Confirm identity'}
          </Button>
        </form>}
      />
    </Card>}

    {suppression && <Card title="Router-write status">
      <Notice
        tone={suppression.active ? 'warning' : 'accent'}
        component="Restore safety gate"
        summary={suppression.active
          ? 'Router writes are suppressed after a controller restore. They stay blocked until an owner explicitly resumes them.'
          : 'No restore-based router-write suppression is active.'}
        details={suppression.active ? <div className="diagnostics-disclosure">
          <div><code>restore_id={suppression.restore_id}</code></div>
          <div>Recorded {new Date(suppression.created_at).toLocaleString()} · {suppression.reason}</div>
          <div>Resuming immediately re-enables automatic 802.11k neighbour maintenance. The reconciler may write hostapd RRM neighbour state; restored desired configuration is still not automatically Applied.</div>
        </div> : 'This status comes from the controller restore-suppression endpoint.'}
        actions={suppression.active ? <Button kind="primary" disabled={busy !== ''} onClick={openResume}>
          Review router-write resumption
        </Button> : undefined}
      />
      {resumeOpen && suppression.active && <Notice
        tone="critical"
        component="Resume router writes"
        summary="Clearing this gate immediately re-enables automatic 802.11k neighbour maintenance, which may write RRM neighbour state."
        details={`This does not start a desired-configuration Apply. Type ${resumeConfirmation} exactly; the active restore id is bound by the controller.`}
        defaultOpen
        actions={<form className="restore-secret-form" onSubmit={resumeRouterWrites}>
          <label className="checkline restore-ack">
            <input type="checkbox" checked={resumeNeighbourAcknowledged} disabled={busy !== ''}
              onChange={(event) => { setFormIssue(''); setResumeNeighbourAcknowledged(event.target.checked) }} />
            I understand that resuming may immediately write 802.11k RRM neighbour state.
          </label>
          <Field label={`Type ${resumeConfirmation} exactly`} value={resumeTypedConfirmation}
            autoComplete="off" required disabled={busy !== ''}
            onChange={(event) => { setFormIssue(''); setResumeTypedConfirmation(event.target.value) }} />
          <div className="notice-actions">
            <Button type="submit" kind="primary" disabled={busy !== '' || !resumeNeighbourAcknowledged || resumeTypedConfirmation !== resumeConfirmation}>
              {busy === 'resume' ? 'Resuming…' : 'Resume router writes'}
            </Button>
            <Button disabled={busy !== ''} onClick={clearSecrets}>Cancel</Button>
          </div>
        </form>}
      />}
    </Card>}

    <Card title="Controller restore" actions={<span className="diagnostics-mode">Preview first · no router calls</span>}>
      {busy === 'load' && !descriptor && !descriptorIssue && !error
        ? <div role="status">Loading restore disclosure…</div>
        : descriptor && <>
          <Notice
            tone="accent"
            component="Portable controller restore"
            summary="Upload an encrypted .oowrtbak, authenticate it, and inspect a disposable migration before any restore is offered."
            details={<div className="diagnostics-disclosure">
              <div>{descriptor.disclosure.summary}</div>
              <dl className="diagnostics-flags">
                <div><dt>Upload/preview controller changes</dt><dd><code>{String(descriptor.disclosure.live_controller_changes)}</code></dd></div>
                <div><dt>Router management calls</dt><dd><code>{String(descriptor.disclosure.router_management_calls)}</code></dd></div>
                <div><dt>Automatic router Apply</dt><dd><code>{String(descriptor.disclosure.automatic_router_apply)}</code></dd></div>
              </dl>
              <div>Maximum artifact: {formatBytes(descriptor.limits.max_upload_bytes)}. Preview expires after {descriptor.limits.retention_seconds} seconds; confirmation has a {descriptor.limits.confirmation_timeout_seconds}-second limit.</div>
            </div>}
            actions={<Button disabled={busy !== ''} onClick={() => void load()}>Refresh restore state</Button>}
          />

          <form className="restore-upload-form" onSubmit={uploadArtifact}>
            <label className="restore-file-field">
              <span>Encrypted backup file</span>
              <input ref={artifactInput} type="file" accept=".oowrtbak,application/vnd.oonfeewrt.backup"
                disabled={busy !== '' || active} onChange={(event) => {
                  setArtifact(event.currentTarget.files?.[0] ?? null)
                  setFormIssue('')
                }} />
            </label>
            <Button type="submit" kind="primary" disabled={busy !== '' || active || !artifact}>
              {busy === 'upload' ? 'Uploading…' : 'Upload encrypted backup'}
            </Button>
          </form>
          {upload && <Notice
            component="Stored encrypted upload"
            summary={`${formatBytes(upload.size_bytes)} encrypted artifact · expires ${new Date(upload.expires_at).toLocaleString()}`}
            details={<div className="diagnostics-disclosure">
              <div><code>sha256={upload.sha256}</code></div>
              <div>Uploading does not decrypt the artifact or change live controller state.</div>
            </div>}
            actions={!active && !completed ? <Button kind="primary" disabled={busy !== ''} onClick={() => {
              clearSecrets()
              if (!recentlyReauthenticated()) requireReauthentication('preview')
              else setPreviewOpen(true)
            }}>Authenticate and preview</Button> : undefined}
          />}

          {previewOpen && upload && <Notice
            component="Restore preview authentication"
            summary="Enter the backup's separate export passphrase. It is cleared before the asynchronous preview starts."
            details="The preview authenticates the fixed artifact, migrates only disposable state, and validates secrets and a usable owner."
            defaultOpen
            actions={<form className="restore-secret-form" onSubmit={startPreview}>
              <Field label="Backup export passphrase" type="password" value={previewPassphrase}
                autoComplete="off" required disabled={busy !== ''}
                onChange={(event) => { setFormIssue(''); setPreviewPassphrase(event.target.value) }} />
              <div className="notice-actions">
                <Button type="submit" kind="primary" disabled={busy !== '' || !previewPassphrase}>
                  {busy === 'preview' ? 'Starting…' : 'Start restore preview'}
                </Button>
                <Button disabled={busy !== ''} onClick={clearSecrets}>Cancel</Button>
              </div>
            </form>}
          />}

          {active && preview && <div className="diagnostics-active" aria-live="polite">
            <div className="diagnostics-active-heading">
              <div><strong>Restore preview {preview.state}</strong><div>{preview.phase}</div></div>
              <div className="notice-actions">
                <Button disabled={busy !== '' || polling.current} onClick={() => void poll(true)}>Refresh preview status</Button>
                <Button disabled={busy !== ''} onClick={() => void cancelPreview()}>
                  {busy === 'cancel' ? 'Cancelling…' : 'Cancel preview'}
                </Button>
              </div>
            </div>
            <div className="diagnostics-progress" role="progressbar" aria-label="Controller restore preview progress"
              aria-valuemin={0} aria-valuemax={100} aria-valuenow={preview.progress_percent}
              aria-valuetext={`${preview.phase}, ${preview.progress_percent}%`}>
              <span style={{ width: `${preview.progress_percent}%` }} />
            </div>
            {pollStopped && <div className="diagnostics-limit-note">Automatic status polling stopped after an error. Refresh explicitly; no retry ran.</div>}
          </div>}

          {preview && restoreTerminalStates.has(preview.state) && preview.state !== 'completed' && <Notice
            tone={preview.state === 'failed' ? 'critical' : 'warning'}
            component="Restore preview"
            summary={preview.state === 'failed' ? preview.error ?? 'The artifact failed safe validation.' : 'Preview cancelled. No live state changed.'}
            details="No controller database was replaced and no router was contacted. Upload or preview again only as a new explicit action."
          />}

          {completed && completed.manifest && completed.counts && <Notice
            tone="accent"
            component="Authenticated restore preview"
            summary={`${completed.manifest.controller_version} · schema ${completed.source_schema} → ${completed.target_schema} · ${completed.counts.devices} devices`}
            details={<div className="diagnostics-disclosure">
              <dl className="restore-preview-counts">
                <div><dt>Devices</dt><dd>{completed.counts.devices}</dd></div>
                <div><dt>Credentials</dt><dd>{completed.counts.credentials}</dd></div>
                <div><dt>Owned sections</dt><dd>{completed.counts.owned_sections}</dd></div>
                <div><dt>WLANs</dt><dd>{completed.counts.wlans}</dd></div>
                <div><dt>Meshes</dt><dd>{completed.counts.meshes}</dd></div>
              </dl>
              <div>Created {new Date(completed.manifest.created_at).toLocaleString()} · database {formatBytes(completed.manifest.database_size_bytes)}</div>
              <div><code>plan_id={completed.plan_id}</code></div>
              <div>This is an authenticated disposable preview. Live controller state is still unchanged.</div>
              {suppression?.active && <div>Confirmation is blocked by the prior restore safety gate. Resume router writes, or keep the gate and do not restore.</div>}
            </div>}
            actions={suppression?.active
              ? <Button disabled>Restore blocked by safety gate</Button>
              : <Button kind="primary" disabled={busy !== ''} onClick={openConfirmation}>Review controller restore</Button>}
          />}

          {confirmOpen && completed && <Notice
            tone="critical"
            component="Controller restore confirmation"
            summary="This replaces controller state, restarts the controller, and revokes active sessions. It never applies restored configuration to routers automatically."
            details={<div className="diagnostics-disclosure">
              <div>The four acknowledgements are independent and required. Router writes must remain suppressed after restore until an owner explicitly resumes them.</div>
              <div>After restart, read-only monitoring of restored devices may resume using restored credentials. No router configuration is applied automatically.</div>
              <div><code>plan_id={completed.plan_id}</code></div>
            </div>}
            defaultOpen
            actions={<form className="restore-secret-form" onSubmit={confirm}>
              <Field label="Re-enter backup export passphrase" type="password" value={confirmExportPassphrase}
                autoComplete="off" required disabled={busy !== ''}
                onChange={(event) => { setFormIssue(''); setConfirmExportPassphrase(event.target.value) }} />
              <Field id="restore-runtime-passphrase" label="Destination controller runtime passphrase" type="password" value={runtimePassphrase}
                autoComplete="off" aria-describedby="restore-runtime-passphrase-help" required disabled={busy !== ''}
                onChange={(event) => { setFormIssue(''); setRuntimePassphrase(event.target.value) }} />
              <div id="restore-runtime-passphrase-help" className="diagnostics-limit-note">
                This is the controller boot/keyring secret, not your signed-in account password.
              </div>
              <Field label={`Type ${restoreConfirmation} exactly`} value={typedConfirmation}
                autoComplete="off" required disabled={busy !== ''}
                onChange={(event) => { setFormIssue(''); setTypedConfirmation(event.target.value) }} />
              {[
                'I understand the controller will restart.',
                'I understand all active controller sessions will be revoked.',
                'I understand router writes remain suppressed after restore until an owner explicitly resumes them.',
                'I understand restored desired configuration will not be applied to routers automatically.',
              ].map((label, index) => <label className="checkline restore-ack" key={label}>
                <input type="checkbox" checked={acknowledgements[index]} disabled={busy !== ''}
                  onChange={(event) => setAcknowledgements((current) => current.map((value, item) =>
                    item === index ? event.target.checked : value))} />
                <span>{label}</span>
              </label>)}
              <div className="notice-actions">
                <Button type="submit" kind="primary" disabled={busy !== '' || !confirmExportPassphrase ||
                  !runtimePassphrase || typedConfirmation !== restoreConfirmation || !acknowledgements.every(Boolean)}>
                  {busy === 'confirm' ? 'Accepting…' : 'Restore controller'}
                </Button>
                <Button disabled={busy !== ''} onClick={clearSecrets}>Cancel review</Button>
              </div>
            </form>}
          />}
        </>}
      {!descriptor && (descriptorIssue || error) && <div className="diagnostics-disabled-action">
        <Button kind="primary" disabled>Upload encrypted backup</Button>
        <Button disabled={busy !== ''} onClick={() => void load()}>Refresh restore disclosure</Button>
      </div>}
    </Card>
  </div>
}

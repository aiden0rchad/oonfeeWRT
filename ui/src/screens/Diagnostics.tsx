import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, api } from '../lib/api'
import type { DiagnosticDescriptor, DiagnosticJob, DiagnosticJobState } from '../lib/api'
import { ago } from '../components/Chart'
import { Banner, Button, Card, Notice } from '../components/ui'

const activeStates = new Set<DiagnosticJobState>(['queued', 'collecting', 'generating'])
const terminalStates = new Set<DiagnosticJobState>(['completed', 'failed', 'cancelled'])
const jobStates = new Set<DiagnosticJobState>([...activeStates, ...terminalStates])
const maxDiagnosticArchiveBytes = 17 << 20
const limitKeys = [
  'devices', 'sources', 'events', 'controller_log_input_bytes',
  'controller_log_output_bytes', 'archive_bytes', 'history',
  'retention_seconds', 'collection_timeout_seconds',
] as const
const sectionIDs = ['controller', 'devices', 'coverage', 'events', 'logs'] as const
const excludedSecretClasses = [
  'controller passphrases',
  'password hashes',
  'session and CSRF tokens',
  'router credentials',
  'Wi-Fi keys',
  'private keys and certificates',
  'raw database and keyring',
  'client notes and fixed-address assignments',
] as const

function record(value: unknown): value is Record<string, unknown> {
  return value != null && typeof value === 'object' && !Array.isArray(value)
}

function exactStringSet(value: unknown[], expected: readonly string[]) {
  return value.length === expected.length && new Set(value).size === expected.length &&
    value.every((item) => typeof item === 'string' && expected.includes(item))
}

function jobProblem(value: unknown, maxArchiveBytes: number) {
  if (!record(value)) return 'a job is not an object'
  if (typeof value.id !== 'string' || !value.id) return 'a job identifier is unavailable'
  if (typeof value.state !== 'string' || !jobStates.has(value.state as DiagnosticJobState)) return 'a job state is invalid'
  if (typeof value.phase !== 'string') return 'a job phase is unavailable'
  if (!Number.isSafeInteger(value.progress_percent) || (value.progress_percent as number) < 0 || (value.progress_percent as number) > 100) return 'job progress is invalid'
  if (!Number.isSafeInteger(value.created_at) || (value.created_at as number) < 0) return 'a job timestamp is invalid'
  for (const key of ['started_at', 'finished_at', 'expires_at'] as const) {
    if (value[key] != null && (!Number.isSafeInteger(value[key]) || (value[key] as number) < 0)) return `job ${key} is invalid`
  }
  if (value.size_bytes != null && (!Number.isSafeInteger(value.size_bytes) ||
    (value.size_bytes as number) <= 0 || (value.size_bytes as number) > maxArchiveBytes)) return 'job size_bytes is invalid'
  if (value.error != null && typeof value.error !== 'string') return 'a job error is invalid'
  const created = value.created_at as number
  const started = value.started_at as number | undefined
  const finished = value.finished_at as number | undefined
  const expires = value.expires_at as number | undefined
  if (started != null && started < created) return 'job timestamps are out of order'
  if (finished != null && finished < (started ?? created)) return 'job timestamps are out of order'
  if (expires != null && (finished == null || expires <= finished)) return 'job expiry is invalid'
  const state = value.state as DiagnosticJobState
  if (state !== 'queued' && started == null) return 'a started job is missing started_at'
  if (activeStates.has(state) && (finished != null || expires != null || value.size_bytes != null)) return 'an active job has terminal fields'
  if (terminalStates.has(state) && (finished == null || expires == null)) return 'a terminal job is missing its expiry'
  if (state === 'completed' && value.progress_percent !== 100) return 'a completed job is not at 100%'
  if (state === 'completed' && value.size_bytes == null) return 'a completed job size is unavailable'
  if (state !== 'completed' && value.size_bytes != null) return 'a non-completed job has a bundle size'
  return ''
}

function descriptorProblem(value: unknown) {
  if (!record(value)) return 'the descriptor is not an object'
  if (value.mode !== 'stored') return 'mode=stored is missing'
  if (value.router_management_calls !== false) return 'router_management_calls=false is missing'
  if (value.router_changes !== false) return 'router_changes=false is missing'
  if (!Array.isArray(value.sections) || !exactStringSet(value.sections.map((section) =>
    record(section) ? section.id : null), sectionIDs) || value.sections.some((section) =>
    !record(section) || typeof section.id !== 'string' || !section.id ||
    typeof section.label !== 'string' || !section.label || typeof section.description !== 'string' || !section.description)) {
    return 'the exact included-section disclosure is unavailable'
  }
  if (!Array.isArray(value.excluded_secret_classes) ||
    !exactStringSet(value.excluded_secret_classes, excludedSecretClasses)) {
    return 'the exact excluded-secret disclosure is unavailable'
  }
  const limits = value.limits
  if (!record(limits) || limitKeys.some((key) =>
    !Number.isSafeInteger(limits[key]) || (limits[key] as number) <= 0)) {
    return 'one or more collection limits are unavailable'
  }
  if ((limits.archive_bytes as number) > maxDiagnosticArchiveBytes) {
    return 'the archive limit exceeds this client build\'s safety ceiling'
  }
  if (!record(value.controller_log) || typeof value.controller_log.available !== 'boolean' ||
    !Array.isArray(value.controller_log.gaps) || value.controller_log.gaps.some((gap) => typeof gap !== 'string') ||
    value.controller_log.available === false && value.controller_log.gaps.length === 0) {
    return 'controller-log coverage is incomplete'
  }
  if (!Array.isArray(value.jobs)) return 'job history is unavailable'
  const invalidJob = value.jobs.map((job) => jobProblem(job, limits.archive_bytes as number)).find(Boolean)
  if (invalidJob) return invalidJob
  if (value.jobs.filter((job) => activeStates.has((job as DiagnosticJob).state)).length > 1) return 'more than one active job was returned'
  return ''
}

function errorText(error: unknown) {
  if (error instanceof ApiError && record(error.body) && typeof error.body.message === 'string') {
    return error.body.message
  }
  return error instanceof Error ? error.message : String(error)
}

function agoMilliseconds(timestamp: number | undefined) {
  return timestamp ? ago(Math.floor(timestamp / 1_000)) : 'never'
}

function formatBytes(value: number | undefined) {
  if (value == null) return 'Size unavailable'
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)} GB`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)} MB`
  if (value >= 1_000) return `${(value / 1_000).toFixed(0)} KB`
  return `${value} B`
}

function mergeJob(current: DiagnosticDescriptor | null, job: DiagnosticJob) {
  if (!current) return null
  return {
    ...current,
    jobs: [job, ...current.jobs.filter((item) => item.id !== job.id)]
      .sort((left, right) => right.created_at - left.created_at),
  }
}

export function Diagnostics() {
  const [descriptor, setDescriptor] = useState<DiagnosticDescriptor | null>(null)
  const [descriptorIssue, setDescriptorIssue] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState('')
  const descriptorGeneration = useRef(0)
  const descriptorInFlight = useRef<Promise<DiagnosticDescriptor | null> | null>(null)
  const jobGeneration = useRef(0)
  const jobInFlight = useRef<{ id: string; request: Promise<DiagnosticJob | null> } | null>(null)

  const refreshDescriptor = useCallback(async () => {
    if (descriptorInFlight.current) return descriptorInFlight.current
    jobGeneration.current++
    jobInFlight.current = null
    const generation = ++descriptorGeneration.current
    const request = api.diagnostics().then((next) => {
      if (generation !== descriptorGeneration.current) return null
      const problem = descriptorProblem(next)
      if (problem) {
        setDescriptor(null)
        setDescriptorIssue(`Diagnostics generation is disabled: ${problem}.`)
        return null
      }
      setDescriptor(next)
      setDescriptorIssue('')
      setError('')
      return next
    }).catch((cause) => {
      if (generation === descriptorGeneration.current) setError(errorText(cause))
      return null
    }).finally(() => {
      if (descriptorInFlight.current === request) descriptorInFlight.current = null
    })
    descriptorInFlight.current = request
    return request
  }, [])

  const refreshJob = useCallback(async (id: string) => {
    if (jobInFlight.current?.id === id) return jobInFlight.current.request
    descriptorGeneration.current++
    descriptorInFlight.current = null
    const generation = ++jobGeneration.current
    const request = api.diagnostic(id).then(({ job }) => {
      if (generation !== jobGeneration.current) return null
      const problem = jobProblem(job, descriptor?.limits.archive_bytes ?? 0)
      if (problem) {
        setError(`Diagnostic status is incomplete: ${problem}.`)
        return null
      }
      setDescriptor((current) => mergeJob(current, job))
      setError('')
      if (terminalStates.has(job.state)) void refreshDescriptor()
      return job
    }).catch((cause) => {
      if (generation === jobGeneration.current) setError(errorText(cause))
      return null
    }).finally(() => {
      if (jobInFlight.current?.request === request) jobInFlight.current = null
    })
    jobInFlight.current = { id, request }
    return request
  }, [descriptor?.limits.archive_bytes, refreshDescriptor])

  useEffect(() => {
    void refreshDescriptor()
    return () => {
      descriptorGeneration.current++
      jobGeneration.current++
      descriptorInFlight.current = null
      jobInFlight.current = null
    }
  }, [refreshDescriptor])

  const active = descriptor?.jobs.find((job) => activeStates.has(job.state))
  useEffect(() => {
    if (!active) return
    const timer = window.setInterval(() => void refreshJob(active.id), 1_000)
    return () => {
      window.clearInterval(timer)
      jobGeneration.current++
      jobInFlight.current = null
    }
  }, [active?.id, refreshJob])

  async function start() {
    if (!descriptor || descriptorIssue || error || active) return
    descriptorGeneration.current++
    jobGeneration.current++
    descriptorInFlight.current = null
    jobInFlight.current = null
    setBusy('start')
    setError('')
    try {
      const { job } = await api.startDiagnostics()
      const problem = jobProblem(job, descriptor.limits.archive_bytes)
      if (problem) throw new Error(`Diagnostic start response is incomplete: ${problem}.`)
      setDescriptor((current) => mergeJob(current, job))
      void refreshJob(job.id)
    } catch (cause) {
      setError(errorText(cause))
      void refreshDescriptor()
    } finally {
      setBusy('')
    }
  }

  async function cancel() {
    if (!active || !descriptor) return
    const maxArchiveBytes = descriptor.limits.archive_bytes
    descriptorGeneration.current++
    jobGeneration.current++
    descriptorInFlight.current = null
    jobInFlight.current = null
    setBusy('cancel')
    setError('')
    try {
      const { job } = await api.cancelDiagnostics(active.id)
      const problem = jobProblem(job, maxArchiveBytes)
      if (problem) throw new Error(`Diagnostic cancel response is incomplete: ${problem}.`)
      setDescriptor((current) => mergeJob(current, job))
      if (!terminalStates.has(job.state)) void refreshJob(job.id)
    } catch (cause) {
      setError(errorText(cause))
      void refreshJob(active.id)
    } finally {
      setBusy('')
    }
  }

  async function download(job: DiagnosticJob) {
    if (!descriptor || job.state !== 'completed' || job.size_bytes == null) return
    setBusy(`download:${job.id}`)
    setError('')
    let link: HTMLAnchorElement | null = null
    let url = ''
    try {
      const result = await api.downloadDiagnostics(job.id, descriptor.limits.archive_bytes, job.size_bytes)
      url = URL.createObjectURL(result.blob)
      link = document.createElement('a')
      link.href = url
      link.download = result.filename
      document.body.appendChild(link)
      link.click()
    } catch (cause) {
      setError(errorText(cause))
      void refreshDescriptor()
    } finally {
      link?.remove()
      if (url) window.setTimeout(() => URL.revokeObjectURL(url), 0)
      setBusy('')
    }
  }

  const history = (descriptor?.jobs ?? [])
    .filter((job) => job.id !== active?.id)
    .slice(0, descriptor?.limits.history ?? 0)
  const progress = active ? Math.max(0, Math.min(100, active.progress_percent)) : 0

  return <div className="diagnostics-page">
    {descriptorIssue && <div role="alert"><Banner tone="critical">{descriptorIssue} Refresh before generating a bundle.</Banner></div>}
    {error && <div role="alert"><Banner tone="critical">
      Diagnostics unavailable: {error}. {descriptor ? 'Last confirmed descriptor and job state remain visible.' : ''}
    </Banner></div>}

    <Card title="Diagnostics bundle" actions={<span className="diagnostics-mode">Stored evidence only</span>}>
      {!descriptor && !descriptorIssue && !error ? <div role="status">Loading diagnostics disclosure…</div> : descriptor && <>
        <Notice
          tone="accent"
          component="Stored diagnostics"
          summary="Builds a redacted ZIP from evidence already stored by this controller."
          details={<div className="diagnostics-disclosure">
            <dl className="diagnostics-flags">
              <div><dt>Collection mode</dt><dd><code>mode={descriptor.mode}</code></dd></div>
              <div><dt>Router management calls</dt><dd><code>router_management_calls={String(descriptor.router_management_calls)}</code></dd></div>
              <div><dt>Router changes</dt><dd><code>router_changes={String(descriptor.router_changes)}</code></dd></div>
            </dl>
            <div><strong>Included sections</strong></div>
            <ul>{descriptor.sections.map((section) => <li key={section.id}><strong>{section.label}</strong> — {section.description}</li>)}</ul>
            <div><strong>Excluded secret classes</strong></div>
            <ul>{descriptor.excluded_secret_classes.map((secret) => <li key={secret}>{secret}</li>)}</ul>
            <div className="diagnostics-limit-note">
              Limits: {descriptor.limits.devices} devices, {descriptor.limits.sources} sources, {descriptor.limits.events} events, {formatBytes(descriptor.limits.archive_bytes)} archive, {descriptor.limits.collection_timeout_seconds}s collection timeout, {descriptor.limits.retention_seconds}s retention.
            </div>
            <div>
              Controller log: {descriptor.controller_log.available ? 'available' : 'unavailable'}.
              {descriptor.controller_log.gaps.length > 0 && ` Gaps: ${descriptor.controller_log.gaps.join('; ')}.`}
            </div>
          </div>}
          actions={<>
            <Button kind="primary" disabled={busy !== '' || Boolean(active) || Boolean(error)} onClick={() => void start()}>
              {busy === 'start' ? 'Starting…' : active ? 'Bundle in progress' : 'Generate stored-only bundle'}
            </Button>
            <Button disabled={busy !== ''} onClick={() => void refreshDescriptor()}>Refresh status</Button>
          </>}
        />

        {active && <div className="diagnostics-active" aria-live="polite">
          <div className="diagnostics-active-heading">
            <div>
              <strong>Bundle in progress</strong>
              <div>{active.phase || active.state} · started {agoMilliseconds(active.started_at ?? active.created_at)}</div>
            </div>
            <Button disabled={busy !== ''} onClick={() => void cancel()}>{busy === 'cancel' ? 'Cancelling…' : 'Cancel generation'}</Button>
          </div>
          <div
            className="diagnostics-progress"
            role="progressbar"
            aria-label="Diagnostics bundle progress"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={progress}
            aria-valuetext={`${active.phase || active.state}, ${progress}%`}
          ><span style={{ width: `${progress}%` }} /></div>
        </div>}
      </>}
      {!descriptor && (descriptorIssue || error) && <div className="diagnostics-disabled-action">
        <Button kind="primary" disabled>Generate stored-only bundle</Button>
        <Button onClick={() => void refreshDescriptor()}>Refresh disclosure</Button>
      </div>}
    </Card>

    <Card title="Recent bundles" pad={false}>
      {history.length === 0 ? <div className="diagnostics-empty">
        {descriptor ? 'No completed, failed or cancelled jobs yet.' : 'History unavailable.'}
      </div> : <div className="diagnostics-history">
        {history.map((job) => <div className="diagnostics-history-row" key={job.id}>
          <div>
            <div className="diagnostics-job-title"><strong>{job.state}</strong><span>{job.phase || 'No phase reported'}</span></div>
            <div className="diagnostics-job-detail">
              {agoMilliseconds(job.finished_at ?? job.created_at)} · {formatBytes(job.size_bytes)}
              {job.expires_at ? ` · expires ${new Date(job.expires_at).toLocaleString()}` : ''}
              {job.error ? ` · ${job.error}` : ''}
            </div>
          </div>
          {job.state === 'completed' && <Button
            disabled={busy !== ''}
            onClick={() => void download(job)}
          >{busy === `download:${job.id}` ? 'Preparing download…' : 'Download ZIP'}</Button>}
        </div>)}
      </div>}
    </Card>
  </div>
}

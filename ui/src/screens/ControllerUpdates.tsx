import { useRef } from 'react'
import type { SessionInfo, ControllerUpdateStatus } from '../lib/api'
import { Banner, Button, Card, CopyButton } from '../components/ui'

const upgradeGuide = 'https://aiden0rchad.github.io/oonfeeWRT/installation/upgrades'

function displayTime(value?: string) {
  if (!value) return 'Not checked'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? 'Unavailable' : date.toLocaleString()
}

function stateLabel(status: ControllerUpdateStatus | null, error: string) {
  if (error) return "Couldn't check for updates"
  if (!status) return 'Checking…'
  if (status.state === 'available') return `Update available — ${status.latest_tag}`
  if (status.state === 'current') return `You're up to date — ${status.current_version}`
  if (status.state === 'unknown') return "Can't tell which version this is"
  return "Couldn't check for updates"
}

export function ControllerUpdates({
  status,
  error = '',
  refreshing = false,
  checkNote = false,
  onRefresh,
  session,
}: {
  status: ControllerUpdateStatus | null
  error?: string
  refreshing?: boolean
  checkNote?: boolean
  onRefresh?: () => void
  session: SessionInfo
}) {
  const envBlock = useRef<HTMLElement>(null)
  const composeBlock = useRef<HTMLElement>(null)
  const available = !error && status?.state === 'available' && status.update_available &&
    Boolean(status.latest_tag && status.latest_version && status.release_url)
  const readOnly = session.role === 'viewer'
  const canCheck = session.role === 'owner' || session.role === 'admin'

  return <div className="controller-updates">
    <Card title="Controller release status" actions={
      canCheck && <Button disabled={refreshing} onClick={onRefresh}>{refreshing ? 'Checking…' : 'Check now'}</Button>
    }>
      <div className="controller-update-summary" aria-live="polite">
        <span className="controller-update-state" data-state={error ? 'unavailable' : status?.state ?? 'loading'}>{stateLabel(status, error)}</span>
        <dl className="controller-update-facts">
          <div><dt>Installed version</dt><dd><code>{status?.current_version || 'Unknown'}</code></dd></div>
          <div><dt>Latest release</dt><dd>{status?.latest_tag ? <code>{status.latest_tag}</code> : 'Unavailable'}</dd></div>
          <div><dt>Last checked</dt><dd>{displayTime(status?.checked_at)}</dd></div>
          <div><dt>Next automatic check</dt><dd>{displayTime(status?.next_check_at)}</dd></div>
        </dl>
        {error
          ? <div role="alert"><Banner tone="warning">The browser could not read controller release status: {error}. This installation is not being reported as current.</Banner></div>
          : status && <p>{status.message}</p>}
        {status?.error && <div role="status"><Banner tone="warning">{status.error.message}</Banner></div>}
        {status?.release_url && <p><a href={status.release_url} target="_blank" rel="noreferrer">View trusted release notes for {status.latest_tag}</a></p>}
        {checkNote && !status?.error && !error && <p role="status">Checked a moment ago — try again in a minute.</p>}
        <p className="controller-update-cache-note">Checks run automatically about twice a day.</p>
      </div>
    </Card>

    {available ? <Card title={`Upgrade to ${status!.latest_tag}`}>
      <div className="controller-update-instructions">
        {readOnly && <Banner tone="warning">You have read-only controller access. Ask the deployment owner to review, back up, and perform the upgrade.</Banner>}
        <p>Read the release notes, then follow the steps below.</p>
        <ol>
          <li>Finish Apply, backup, restore, diagnostics, RF scan, and optional-package operations.</li>
          <li>Back up and verify the matching database, keyring, and runtime passphrase. Retain the old image or binary for rollback.</li>
          <li>Keep the existing controller data volume and passphrase. Never run old and new controllers against the same files.</li>
        </ol>
        {status!.install_kind !== 'binary' && <>
          <h3>Docker Compose</h3>
          <p>From the deployment's Compose directory, set the validated release tag in its <code>.env</code>:</p>
          <pre><code ref={envBlock}>{`OONFEE_VERSION=${status!.latest_tag}`}</code></pre>
          <CopyButton target={envBlock} label="Copy release environment line" />
          <p>Then pull that service image and recreate the controller container:</p>
          <pre><code ref={composeBlock}>{`docker compose pull oonfeewrt\ndocker compose up -d oonfeewrt`}</code></pre>
          <CopyButton target={composeBlock} label="Copy Docker Compose commands" />
          <p>Confirm the service is running, check the executable version, and sign in after restart. Preserve the existing volume and passphrase throughout.</p>
        </>}
        {status!.install_kind !== 'container' && <>
          <h3>Standalone binary</h3>
          <p>Download the release archives and checksums from the trusted release link above, verify the checksum, stop the old daemon cleanly, replace the executable, and restart it with the unchanged data directory and passphrase source.</p>
        </>}
        <p><a href={upgradeGuide} target="_blank" rel="noreferrer">Read the complete upgrade and rollback guide</a></p>
      </div>
    </Card> : <Card title="Upgrade guidance">
      <p>No target-specific commands are shown until the controller has validated a strictly newer published stable release.</p>
      <p>For planned binary or container maintenance, use the <a href={upgradeGuide} target="_blank" rel="noreferrer">upgrade and rollback guide</a> and retain a matching recovery unit.</p>
    </Card>}

    <details className="controller-update-scope">
      <summary>What this check does and doesn't do</summary>
      <p>The controller—not this browser—checks the public GitHub Releases API. It sends no credentials, account details, controller identity, or router inventory. GitHub and the network path can still observe the controller's ordinary public request IP and transport metadata.</p>
      <p>This checks only the oonfeeWRT controller executable's published stable version. It is separate from OpenWrt router firmware and cannot establish either the controller's or the routers' security posture.</p>
      <p>Read the release notes for security changes; an update notice does not establish that this controller is vulnerable or secure.</p>
      <p>This does not assess the controller's or the routers' security.</p>
    </details>
  </div>
}

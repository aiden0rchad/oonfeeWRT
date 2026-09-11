import { useEffect, useState } from 'react'
import type { SessionInfo } from '../lib/api'
import { PageHeader } from '../components/ui'
import { Account } from './Account'
import { Accounts } from './Accounts'

export function AccountsPage({
  session,
  onSessionChange,
  onCurrentSessionRevoked,
}: {
  session: SessionInfo
  onSessionChange: (session: SessionInfo) => void
  onCurrentSessionRevoked: () => void
}) {
  const [tab, setTab] = useState<'account' | 'accounts'>('account')
  const canManage = session.role === 'owner'
  const activeTab = canManage ? tab : 'account'
  const tabs = [
    { id: 'account' as const, label: 'My account' },
    ...(canManage ? [{ id: 'accounts' as const, label: 'Manage accounts' }] : []),
  ]

  useEffect(() => {
    if (!canManage) setTab('account')
  }, [canManage])

  return <div className="settings-page">
    <PageHeader title="Accounts" purpose="Your profile, password, sessions, and controller access." />
    <div className="settings-tabs" role="tablist" aria-label="Account sections">
      {tabs.map((item, index) => <button
        key={item.id}
        id={`accounts-tab-${item.id}`}
        type="button"
        role="tab"
        aria-selected={activeTab === item.id}
        aria-controls={`accounts-panel-${item.id}`}
        tabIndex={activeTab === item.id ? 0 : -1}
        onClick={() => setTab(item.id)}
        onKeyDown={(event) => {
          let next = index
          if (event.key === 'ArrowRight') next = (index + 1) % tabs.length
          else if (event.key === 'ArrowLeft') next = (index - 1 + tabs.length) % tabs.length
          else if (event.key === 'Home') next = 0
          else if (event.key === 'End') next = tabs.length - 1
          else return
          event.preventDefault()
          setTab(tabs[next].id)
          requestAnimationFrame(() => document.getElementById(`accounts-tab-${tabs[next].id}`)?.focus())
        }}
      >{item.label}</button>)}
    </div>
    <div
      id={`accounts-panel-${activeTab}`}
      role="tabpanel"
      aria-labelledby={`accounts-tab-${activeTab}`}
      className="settings-panel"
    >
      {activeTab === 'account'
        ? <Account session={session} onCurrentSessionRevoked={onCurrentSessionRevoked} />
        : <Accounts
            session={session}
            onSessionChange={onSessionChange}
            onCurrentSessionRevoked={onCurrentSessionRevoked}
          />}
    </div>
  </div>
}

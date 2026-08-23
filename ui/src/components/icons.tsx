export type NavigationIconName =
  | 'dashboard'
  | 'topology'
  | 'radios'
  | 'devices'
  | 'clients'
  | 'policy'
  | 'settings'
  | 'adopt'
  | 'logs'
  | 'expand'
  | 'collapse'

export function NavigationIcon({ name }: { name: NavigationIconName }) {
  const content = (() => {
    switch (name) {
      case 'dashboard':
        return <>
          <rect x="3.5" y="3.5" width="7" height="7" rx="1.25" />
          <rect x="13.5" y="3.5" width="7" height="4.5" rx="1.25" />
          <rect x="13.5" y="11" width="7" height="9.5" rx="1.25" />
          <rect x="3.5" y="13.5" width="7" height="7" rx="1.25" />
        </>
      case 'topology':
        return <>
          <path d="M12 7.5V5M8.2 11l-2.3-1.4M15.8 11l2.3-1.4M8.2 15l-2.3 1.4M15.8 15l2.3 1.4" />
          <circle cx="12" cy="12.5" r="3" />
          <circle cx="12" cy="3.5" r="1.5" />
          <circle cx="4.5" cy="8.7" r="1.5" />
          <circle cx="19.5" cy="8.7" r="1.5" />
          <circle cx="4.5" cy="17.3" r="1.5" />
          <circle cx="19.5" cy="17.3" r="1.5" />
        </>
      case 'radios':
        return <>
          <path d="M4.4 9a10.8 10.8 0 0 1 15.2 0M7.2 12a6.8 6.8 0 0 1 9.6 0M10 15a2.9 2.9 0 0 1 4 0" />
          <circle cx="12" cy="18" r="1.25" fill="currentColor" stroke="none" />
        </>
      case 'devices':
        return <>
          <path d="M7 8V4.5M17 8V4.5" />
          <rect x="3.5" y="8" width="17" height="11" rx="2" />
          <path d="M7 15.5h5.5" />
          <circle cx="16.5" cy="15.5" r="1" fill="currentColor" stroke="none" />
        </>
      case 'clients':
        return <>
          <rect x="4" y="4" width="16" height="12" rx="1.75" />
          <path d="M2.75 20h18.5M8.5 16l-1 4M15.5 16l1 4" />
        </>
      case 'policy':
        return <>
          <path d="M12 2.8 19 5.7v5.6c0 4.5-2.8 8-7 9.9-4.2-1.9-7-5.4-7-9.9V5.7L12 2.8Z" />
          <path d="m8.8 12 2.1 2.1 4.4-4.6" />
        </>
      case 'settings':
        return <>
          <circle cx="12" cy="12" r="4" />
          <circle cx="12" cy="12" r="1.25" fill="currentColor" stroke="none" />
          <path d="M12 2.8v3M12 18.2v3M2.8 12h3M18.2 12h3M5.5 5.5l2.1 2.1M16.4 16.4l2.1 2.1M18.5 5.5l-2.1 2.1M7.6 16.4l-2.1 2.1" />
        </>
      case 'adopt':
        return <>
          <rect x="3.5" y="7" width="13.5" height="12" rx="2" />
          <path d="M7 15h4M19 3.5v7M15.5 7h7" />
        </>
      case 'logs':
        return <>
          <path d="M6 3.5h8l4 4V20.5H6Z" />
          <path d="M14 3.5v4h4M9 11h6M9 14.5h6M9 18h4" />
        </>
      case 'expand':
        return <>
          <rect x="3.5" y="4" width="17" height="16" rx="2" />
          <path d="M8 4v16M12 9l3 3-3 3" />
        </>
      case 'collapse':
        return <>
          <rect x="3.5" y="4" width="17" height="16" rx="2" />
          <path d="M8 4v16M15 9l-3 3 3 3" />
        </>
    }
  })()

  return (
    <svg
      aria-hidden="true"
      focusable="false"
      width="24"
      height="24"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      {content}
    </svg>
  )
}

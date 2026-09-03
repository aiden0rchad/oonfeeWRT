import { defineConfig } from 'vitepress'

const start = [
  { text: 'What oonfeeWRT does', link: '/getting-started/' },
  { text: 'Quick start', link: '/getting-started/quick-start' },
  { text: 'Adopt your first device', link: '/getting-started/first-adoption' },
]

const install = [
  { text: 'Standalone binary', link: '/installation/binary' },
  { text: 'Docker Compose', link: '/installation/docker' },
  { text: 'Reverse proxy and TLS', link: '/installation/reverse-proxy' },
  { text: 'Upgrade and roll back', link: '/installation/upgrades' },
]

const use = [
  { text: 'Dashboard and speed tests', link: '/guide/dashboard' },
  { text: 'Discovery, adoption, and devices', link: '/guide/devices' },
  { text: 'Networks, VLANs, and DHCP', link: '/guide/networks' },
  { text: 'Wi-Fi, roaming, and overrides', link: '/guide/wifi' },
  { text: 'Radios and channel planning', link: '/guide/radios' },
  { text: 'Clients and topology', link: '/guide/clients-topology' },
  { text: 'Policy Engine and firewall', link: '/guide/policy-engine' },
  { text: 'Logs and diagnostics', link: '/guide/logs-diagnostics' },
]

const operate = [
  { text: 'Accounts, roles, and sessions', link: '/operations/accounts' },
  { text: 'Backup and staged restore', link: '/operations/backups' },
  { text: 'Routine maintenance', link: '/operations/maintenance' },
]

const understand = [
  { text: 'How the controller works', link: '/concepts/architecture' },
  { text: 'Safety and ownership model', link: '/concepts/safety' },
  { text: 'Telemetry and retention', link: '/concepts/data-retention' },
  { text: 'Permissions', link: '/concepts/permissions' },
]

const reference = [
  { text: 'Requirements and compatibility', link: '/reference/requirements' },
  { text: 'CLI and environment', link: '/reference/cli' },
  { text: 'Capability and support matrix', link: '/reference/capabilities' },
  { text: 'Troubleshooting', link: '/reference/troubleshooting' },
  { text: 'FAQ', link: '/reference/faq' },
  { text: 'Engineering reference', link: '/reference/engineering' },
  { text: 'Release notes', link: '/reference/releases' },
]

export default defineConfig({
  lang: 'en-US',
  title: 'oonfeeWRT',
  titleTemplate: ':title | oonfeeWRT Docs',
  description: 'Documentation for oonfeeWRT v0.1.4: install, configure, operate, and understand the OpenWrt controller.',
  base: '/oonfeeWRT/',
  cleanUrls: true,
  lastUpdated: true,
  sitemap: { hostname: 'https://aiden0rchad.github.io/oonfeeWRT/' },
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/oonfeeWRT/favicon.svg' }],
    ['meta', { name: 'theme-color', content: '#f5f6f8', media: '(prefers-color-scheme: light)' }],
    ['meta', { name: 'theme-color', content: '#0f1114', media: '(prefers-color-scheme: dark)' }],
    ['meta', { property: 'og:type', content: 'website' }],
    ['meta', { property: 'og:title', content: 'oonfeeWRT v0.1.4 Documentation' }],
    ['meta', { property: 'og:description', content: 'Self-hosted management for stock OpenWrt, with safe configuration, compatibility reporting, and evidence-backed WAN telemetry.' }],
    ['meta', { property: 'og:image', content: 'https://aiden0rchad.github.io/oonfeeWRT/social-card.svg' }],
  ],
  markdown: {
    lineNumbers: true,
  },
  themeConfig: {
    logo: {
      light: '/logo-light.svg',
      dark: '/logo-dark.svg',
      alt: 'oonfeeWRT',
    },
    siteTitle: 'oonfeeWRT Docs',
    nav: [
      { text: 'Start here', link: '/getting-started/' },
      { text: 'Guides', link: '/guide/dashboard' },
      { text: 'Operations', link: '/operations/accounts' },
      { text: 'Reference', link: '/reference/requirements' },
      {
        text: 'v0.1.4',
        items: [
          { text: 'Release notes', link: '/reference/releases' },
          { text: 'Download v0.1.4', link: 'https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.4' },
        ],
      },
    ],
    sidebar: [
      { text: 'Start here', items: start },
      { text: 'Install and upgrade', collapsed: false, items: install },
      { text: 'Use oonfeeWRT', collapsed: false, items: use },
      { text: 'Operate', collapsed: false, items: operate },
      { text: 'Understand', collapsed: true, items: understand },
      { text: 'Reference and help', collapsed: true, items: reference },
    ],
    search: { provider: 'local' },
    outline: { level: [2, 3], label: 'On this page' },
    docFooter: { prev: 'Previous', next: 'Next' },
    editLink: {
      pattern: 'https://github.com/aiden0rchad/oonfeeWRT/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },
    lastUpdated: {
      text: 'Last updated',
      formatOptions: { dateStyle: 'medium' },
    },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/aiden0rchad/oonfeeWRT' },
    ],
    footer: {
      message: 'Self-hosted management for stock OpenWrt. No cloud broker. No custom firmware.',
      copyright: 'Apache-2.0 · oonfeeWRT contributors',
    },
    darkModeSwitchLabel: 'Theme',
    sidebarMenuLabel: 'Menu',
    returnToTopLabel: 'Back to top',
    externalLinkIcon: true,
  },
})

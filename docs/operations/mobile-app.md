# Mobile access and installing the web app

::: info Added in v0.1.6
The installable-app manifest, offline guidance, and mobile navigation drawer
are included in v0.1.6. Installation does not add offline access to private data.
:::

oonfeeWRT remains a responsive web application. Installing it adds a launcher
and standalone window; it does not install the controller on your phone,
create a cloud account, open firewall ports, or bypass controller sign-in.

## Use the mobile layout

At narrow widths, **Open navigation** in the top bar opens the complete
sidebar. Choose a workspace, select **Close navigation**, tap the backdrop, or
press Escape to close it. Keyboard focus stays inside the open navigation and
returns to the menu button when dismissed. Pages use the full available width
when the drawer is closed. Settings, Accounts, and Logs remain in the
Controller group at the end of the navigation.

## Install on a supported browser

1. Connect to the controller's management network or your existing VPN.
2. Open its usual URL. Use [HTTPS](../installation/reverse-proxy.md) for a
   controller on another machine; loopback URLs work for local development.
3. Use the browser's **Install app**, **Add to Home Screen**, or **Add to Dock**
   action when available. Browser and operating-system support vary.
4. Launch oonfeeWRT from its new icon and sign in normally.

The app includes 192px/512px icons and a standalone-display manifest. HTTP on a
remote LAN IP can still serve the ordinary UI, but does not meet installability
requirements in browsers that require a secure origin. See
[MDN's installation requirements](https://developer.mozilla.org/en-US/docs/Web/Progressive_web_apps/Guides/Making_PWAs_installable)
for current browser behavior.

## When the controller is unreachable

The installed app can show a static explanation asking you to reconnect to the
management network or VPN and check the controller process. It does **not**
show a cached copy of your private network inventory, queue configuration
changes, or claim an operation succeeded offline. Use **Try again** after
restoring access.

The service worker does not cache API responses, authenticated HTML, or
credentials. API requests, mutations, assets, and other origins are not
intercepted by its offline-navigation handler. There is no offline controller
or background management mode.

## Upgrades and removal

Update the controller normally. Its embedded UI, service worker, and manifest
are served by the same binary. The worker and manifest are revalidated rather
than pinned as immutable assets. No router change results from installing,
updating, or removing the app shortcut.

Remove the installed application through the browser or operating system.
Removing a shortcut does not revoke a controller session; use
[Accounts → My account](./accounts.md) to review and revoke sessions when
needed.

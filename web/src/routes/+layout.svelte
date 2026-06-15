<script lang="ts">
  import "../app.css";

  import { goto } from "$app/navigation";
  import { page } from "$app/state";
  import { Avatar, Container, IconButton, Separator, ToastProvider } from "@qovira/ui";
  import { getTheme, subscribe, toggleTheme } from "@qovira/theme/runtime";
  import type { Theme } from "@qovira/theme/runtime";
  import { BellIcon, ChatsIcon, MoonIcon, PushPinIcon, PushPinSlashIcon, SunIcon } from "phosphor-svelte";
  import { onMount } from "svelte";
  import type { Snippet } from "svelte";

  import { Api, onUnauthorized } from "$lib/api/index.js";
  import { isExemptRoute, shouldRedirectToLogin } from "$lib/auth/guard.js";
  import { getRailPinned, initPrefs, setRailPinned } from "$lib/stores/ui-preferences.svelte.js";
  import {
    isAuthenticated,
    notifySessionReady,
    notifyTearDown,
    onSessionReady,
    onTearDown,
    resetSession,
    seedSession,
  } from "$lib/stores/session.svelte.js";
  import { resetReminders } from "$lib/stores/reminders.svelte.js";
  import { resetConversation } from "$lib/stores/conversation.svelte.js";
  import { resetToolCalls } from "$lib/stores/tool-calls.svelte.js";
  import { resetConfirmations } from "$lib/stores/confirmations.svelte.js";
  import { openSseConnection, closeSseConnection } from "$lib/sse/client.js";
  import RailEntry from "$lib/components/RailEntry.svelte";
  import {
    nav_aria_label,
    nav_loading,
    nav_account,
    nav_pin,
    nav_unpin,
    nav_switch_to_evening,
    nav_switch_to_daylight,
    nav_chat,
    nav_reminders,
  } from "$lib/paraglide/messages.js";

  interface Props {
    children: Snippet;
  }

  const { children }: Props = $props();

  // ---------------------------------------------------------------------------
  // Rail state: collapsed (default) / peek (hover/focus, transient) / pinned
  // ---------------------------------------------------------------------------
  // pinned is stored in the ui-preferences singleton.
  // expanded tracks the transient peek state (hover/keyboard-focus).
  let expanded = $state(false);

  // Derived: the rail is visually open when pinned OR peeking.
  const railOpen = $derived(getRailPinned() || expanded);

  function handleMouseEnter(): void {
    if (!getRailPinned()) expanded = true;
  }

  function handleMouseLeave(): void {
    if (!getRailPinned()) expanded = false;
  }

  function handleFocusIn(): void {
    if (!getRailPinned()) expanded = true;
  }

  function handleFocusOut(event: FocusEvent): void {
    if (getRailPinned()) return;
    // Only collapse when focus leaves the rail entirely (relatedTarget not within the nav).
    const related = event.relatedTarget;
    const nav = event.currentTarget as HTMLElement | null;
    if (nav && related instanceof Node && nav.contains(related)) return;
    expanded = false;
  }

  function togglePin(): void {
    setRailPinned(!getRailPinned());
    // When pinning, lock open; when unpinning, collapse (peek may re-open on hover).
    if (!getRailPinned()) expanded = false;
  }

  // ---------------------------------------------------------------------------
  // Theme
  // ---------------------------------------------------------------------------
  let currentTheme = $state<Theme>("daylight");

  onMount(() => {
    currentTheme = getTheme();
    const unsub = subscribe((t) => {
      currentTheme = t;
    });
    return unsub;
  });

  function handleToggleTheme(): void {
    toggleTheme();
    // currentTheme is updated via the subscribe callback.
  }

  // ---------------------------------------------------------------------------
  // Active route detection
  // ---------------------------------------------------------------------------
  // Fix B: boundary-safe segment match so e.g. /reminders does not match
  // a future /reminders-archive.
  function isActive(href: string): boolean {
    if (href === "/") return page.url.pathname === "/";
    return page.url.pathname === href || page.url.pathname.startsWith(href + "/");
  }

  // ---------------------------------------------------------------------------
  // Boot probe + route guard + 401 handler — wired once on mount
  // ---------------------------------------------------------------------------
  // `booted` gates rendering of guarded routes until the /me probe settles, so
  // an authenticated reload never flashes the bare page (no shell) and an
  // unauthenticated load never flashes guarded content before the redirect.
  // Exempt routes (/login, /onboarding/*) render immediately, regardless.
  let booted = $state(false);

  onMount(() => {
    // Preferences are browser-only side effects.
    initPrefs();

    // Wire the SSE lifecycle hooks before the boot probe so the connection opens
    // immediately when the session is confirmed (either via /me boot probe or login).
    onSessionReady(openSseConnection);
    onTearDown(() => {
      closeSseConnection();
      // Reset per-user live stores so the next session starts clean.
      resetReminders();
      resetConversation();
      resetToolCalls();
      resetConfirmations();
    });

    // Register the centralised 401 handler. This is the single authority for
    // "expired or revoked session": clear state, tear down SSE, bounce to /login.
    onUnauthorized(() => {
      notifyTearDown();
      resetSession();
      void goto("/login");
    });

    // Boot probe: GET /me — 200 → authenticated, 401 → redirect to /login.
    // The probe runs on every fresh page load; the session cookie rides
    // automatically (HttpOnly, credentials: "include"). The token is never
    // read or stored by the client.
    void (async () => {
      try {
        const { data } = await Api.GET("/me", {});

        if (data) {
          // 200: seed the session store with the user. The server does not return
          // expiresAt on /me, so we seed it null — the soft pre-expiry seam stays
          // disarmed until a login (which carries the real expiry) re-seeds it.
          seedSession({ user: data.user, expiresAt: null });
          // Open the SSE connection — the boot-probe path is a valid session start.
          notifySessionReady();
        }
        // 401 is handled by the onUnauthorized hook above (clears session, redirects).

        // Guard: after the probe settles, redirect if still unauthenticated on a
        // guarded route.
        if (shouldRedirectToLogin(page.url.pathname, isAuthenticated())) {
          await goto("/login");
        }
      } finally {
        // The probe has settled (success, 401, or network error): rendering of
        // guarded routes can now resolve to either the shell or the redirect.
        booted = true;
      }
    })();
  });
</script>

<ToastProvider>
  <!--
    Render order:
      1. Exempt routes (/login, /onboarding/*) render their children directly,
         without the shell and without waiting on the boot probe.
      2. On a guarded route, render a quiet splash until the /me probe settles
         (`booted`) — no flash of bare page content or guarded content.
      3. Probe settled + authenticated → the app shell (nav rail + content).
      4. Probe settled + unauthenticated → the boot probe is redirecting to
         /login; render the same quiet splash until navigation completes.
  -->
  {#if isExemptRoute(page.url.pathname)}
    {@render children()}
  {:else if !booted}
    <div class="flex h-screen items-center justify-center">
      <span class="sr-only">{nav_loading()}</span>
    </div>
  {:else if isAuthenticated()}
    <div class="flex h-screen overflow-hidden">
      <!--
        Rail — slim navigation sidebar.
        Width transitions honor prefers-reduced-motion via the CSS custom property.
      -->
      <nav
        aria-label={nav_aria_label()}
        class="rail bg-surface border-border flex shrink-0 flex-col border-r transition-[width] duration-200 ease-in-out motion-reduce:transition-none {railOpen
          ? 'w-[200px]'
          : 'w-[56px]'}"
        onmouseenter={handleMouseEnter}
        onmouseleave={handleMouseLeave}
        onfocusin={handleFocusIn}
        onfocusout={handleFocusOut}
      >
        <!-- Main nav entries -->
        <!--
          Fix A: each entry renders a single persistent <a> via RailEntry.
          The anchor is NEVER recreated on expand/collapse — focus is preserved.
          RailEntry keeps the Tooltip mounted and gates its open state to suppress
          the popup when the rail is already expanded (label visible in DOM).
        -->
        <ul class="flex flex-1 flex-col gap-1 p-2" role="list">
          <li>
            <RailEntry href="/" label={nav_chat()} icon={ChatsIcon} active={isActive("/")} {expanded} />
          </li>
          <li>
            <RailEntry
              href="/reminders"
              label={nav_reminders()}
              icon={BellIcon}
              active={isActive("/reminders")}
              {expanded}
            />
          </li>
        </ul>

        <!-- Rail footer: pin toggle, account affordance, theme toggle -->
        <div class="flex flex-col gap-1 border-t border-inherit p-2">
          <!-- Pin / unpin the rail -->
          <div class="flex justify-end">
            <IconButton
              icon={getRailPinned() ? PushPinSlashIcon : PushPinIcon}
              label={getRailPinned() ? nav_unpin() : nav_pin()}
              variant="ghost"
              size="md"
              onclick={togglePin}
            />
          </div>

          <Separator />

          <!--
            Fix D: drop aria-label so visible text and accessible name agree.
            The label span is always in the DOM; visibility is toggled by CSS
            (hidden when collapsed, visible when expanded) — same pattern as RailEntry.
          -->
          <a
            href="/settings"
            class="flex h-10 items-center gap-3 rounded px-2 transition-colors hover:bg-surface-raised
                   focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-current"
          >
            <Avatar name="User" size="sm" />
            <span class={railOpen ? "text-text text-sm" : "sr-only"}>{nav_account()}</span>
          </a>

          <!-- Theme toggle -->
          <IconButton
            icon={currentTheme === "daylight" ? MoonIcon : SunIcon}
            label={currentTheme === "daylight" ? nav_switch_to_evening() : nav_switch_to_daylight()}
            variant="ghost"
            size="md"
            onclick={handleToggleTheme}
          />
        </div>
      </nav>

      <!-- Content column -->
      <main class="flex flex-1 flex-col overflow-y-auto">
        <Container width="content" class="px-6 py-6">
          {@render children()}
        </Container>
      </main>
    </div>
  {:else}
    <!-- Unauthenticated on a guarded route: the boot probe is redirecting to /login. -->
    <div class="flex h-screen items-center justify-center">
      <span class="sr-only">{nav_loading()}</span>
    </div>
  {/if}
</ToastProvider>

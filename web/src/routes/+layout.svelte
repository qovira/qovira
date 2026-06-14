<script lang="ts">
  import "../app.css";

  import { page } from "$app/state";
  import { Avatar, Container, IconButton, Separator, ToastProvider } from "@qovira/ui";
  import { getTheme, subscribe, toggleTheme } from "@qovira/theme/runtime";
  import type { Theme } from "@qovira/theme/runtime";
  import { BellIcon, ChatsIcon, MoonIcon, PushPinIcon, PushPinSlashIcon, SunIcon } from "phosphor-svelte";
  import { onMount } from "svelte";
  import type { Snippet } from "svelte";

  import { getRailPinned, initPrefs, setRailPinned } from "$lib/stores/ui-preferences.svelte.js";
  import RailEntry from "$lib/components/RailEntry.svelte";

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
  // Fix C: one-shot browser-only init via onMount (not $effect)
  // ---------------------------------------------------------------------------
  onMount(() => {
    initPrefs();
  });
</script>

<ToastProvider>
  <div class="flex h-screen overflow-hidden">
    <!--
      Rail — slim navigation sidebar.
      Width transitions honor prefers-reduced-motion via the CSS custom property.
    -->
    <nav
      aria-label="Main navigation"
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
          <RailEntry href="/" label="Chat" icon={ChatsIcon} active={isActive("/")} {expanded} />
        </li>
        <li>
          <RailEntry href="/reminders" label="Reminders" icon={BellIcon} active={isActive("/reminders")} {expanded} />
        </li>
      </ul>

      <!-- Rail footer: pin toggle, account affordance, theme toggle -->
      <div class="flex flex-col gap-1 border-t border-inherit p-2">
        <!-- Pin / unpin the rail -->
        <div class="flex justify-end">
          <IconButton
            icon={getRailPinned() ? PushPinSlashIcon : PushPinIcon}
            label={getRailPinned() ? "Unpin navigation" : "Pin navigation"}
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
          <span class={railOpen ? "text-text text-sm" : "sr-only"}>Account</span>
        </a>

        <!-- Theme toggle -->
        <IconButton
          icon={currentTheme === "daylight" ? MoonIcon : SunIcon}
          label={currentTheme === "daylight" ? "Switch to Evening" : "Switch to Daylight"}
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
</ToastProvider>

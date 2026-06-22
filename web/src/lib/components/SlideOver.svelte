<script lang="ts">
  // SlideOver — a minimal accessible slide-over panel.
  //
  // NOTE: @qovira/ui (v2.0.0) ships Modal (center dialog) and Popover but no
  // dedicated slide-over / sheet / drawer primitive. This component fills that
  // gap with the minimum required for accessibility: focus-trap (native dialog
  // element), Escape to close, role="dialog" + aria-modal, and theme tokens only.
  // Report: this is a gap in @qovira/ui — a Sheet/SlideOver primitive should be
  // added upstream so consumers don't need to build their own.
  //
  // Props:
  //   open        — bindable; controls visibility.
  //   title       — accessible dialog title (rendered as <h2> and referenced via aria-labelledby).
  //   onclose     — callback prop called when the panel closes (Esc, overlay click).
  //   children    — panel body content (snippet).

  import type { Snippet } from "svelte";

  interface Props {
    open?: boolean;
    title: string;
    onclose?: () => void;
    children: Snippet;
  }

  let { open = $bindable(false), title, onclose, children }: Props = $props();

  // ---------------------------------------------------------------------------
  // Dialog element ref — used to call .showModal() / .close() on the native
  // <dialog> element, which gives us a browser-native focus trap for free.
  // ---------------------------------------------------------------------------
  let dialogEl = $state<HTMLDialogElement | null>(null);

  // ---------------------------------------------------------------------------
  // Focus restore — capture the trigger element when the dialog opens, restore
  // it on close so keyboard users land back where they started.
  //
  // Native <dialog>.close() does not restore focus when the dialog was opened
  // programmatically via .showModal() triggered by a prop change (as opposed to
  // being opened by a user gesture directly on the <dialog>). We handle it
  // manually so Escape, backdrop-click, and selection all restore correctly.
  //
  // The new-conversation path is safe: ConversationSwitcher calls focusComposer()
  // AFTER close(), so the composer focus wins over this restore (last writer wins).
  // ---------------------------------------------------------------------------
  let previouslyFocused = $state<Element | null>(null);

  // Sync the `open` prop to the native dialog open/close calls.
  // $effect is appropriate here: syncing with a DOM element (outside Svelte).
  $effect(() => {
    if (dialogEl === null) return;
    if (open) {
      // Capture the currently-focused element before the dialog steals focus.
      previouslyFocused = document.activeElement;
      if (!dialogEl.open) dialogEl.showModal();
    } else {
      if (dialogEl.open) dialogEl.close();
    }
  });

  // ---------------------------------------------------------------------------
  // Event handlers
  // ---------------------------------------------------------------------------

  function handleClose(): void {
    open = false;
    // Restore focus to the element that opened the dialog, before calling
    // onclose. This covers Escape, backdrop-click, and the close button.
    // focusComposer() (called by ConversationSwitcher after close) will
    // override this restore when the new-conversation path is taken.
    if (previouslyFocused instanceof HTMLElement) {
      previouslyFocused.focus();
    }
    previouslyFocused = null;
    onclose?.();
  }

  function handleCancel(event: Event): void {
    // Esc key fires the native "cancel" event on <dialog>. preventDefault so
    // we can control the state transition through our `open` prop.
    event.preventDefault();
    handleClose();
  }

  function handleOverlayClick(event: MouseEvent): void {
    // Close when clicking the backdrop (the <dialog> element itself, not its content).
    if (event.target === dialogEl) {
      handleClose();
    }
  }
</script>

<!--
  Native <dialog> gives us:
    - Browser-native focus trap (no polyfill needed)
    - Escape key via the "cancel" event
    - role="dialog" + aria-modal semantics automatically
    - showModal() prevents background interaction

  The panel slides in from the right using a CSS transform. Theme tokens only —
  no hardcoded colors. The backdrop is styled via ::backdrop (CSS pseudo-element
  on the <dialog>) so we get the native blocking layer without a separate overlay div.

  aria-labelledby references the visible <h2> title so screen readers announce
  the dialog name from the rendered heading rather than a duplicate hidden attribute.
-->
<dialog
  bind:this={dialogEl}
  aria-labelledby="slide-over-title"
  oncancel={handleCancel}
  onclick={handleOverlayClick}
  class="slide-over fixed inset-0 m-0 h-full max-h-full w-full max-w-full bg-transparent p-0
         backdrop:bg-warm-900/40 backdrop:backdrop-blur-sm"
>
  <!--
    Inner panel: slides in from the right. Positioned absolute inside the <dialog>
    so it occupies the right edge while the left side remains as the backdrop hit target.
    Use pointer-events-none on the parent <dialog> to allow clicking the backdrop,
    then restore pointer-events on the panel content itself.
  -->
  <!--
    stopPropagation on the panel content div prevents a click inside the panel
    from bubbling up to the <dialog> backdrop handler. This div is structural
    (not interactive itself) — the actual interactive elements are inside it.
  -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div
    class="absolute inset-y-0 right-0 flex h-full w-full max-w-sm flex-col
           border-l border-border bg-surface shadow-[var(--shadow-lg)]"
    onclick={(e) => {
      e.stopPropagation();
    }}
  >
    <!-- Panel header -->
    <div class="flex shrink-0 items-center justify-between border-b border-border px-4 py-3">
      <h2 id="slide-over-title" class="text-fg text-base font-semibold">{title}</h2>
      <button
        type="button"
        class="text-fg-muted hover:text-fg rounded p-1
               focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-current"
        onclick={handleClose}
        aria-label="Close"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 256 256" aria-hidden="true">
          <path
            fill="currentColor"
            d="M205.66 194.34a8 8 0 0 1-11.32 11.32L128 139.31l-66.34 66.35a8 8 0 0 1-11.32-11.32L116.69 128L50.34 61.66a8 8 0 0 1 11.32-11.32L128 116.69l66.34-66.35a8 8 0 0 1 11.32 11.32L139.31 128Z"
          />
        </svg>
      </button>
    </div>

    <!-- Panel body -->
    <div class="flex-1 overflow-y-auto">
      {@render children()}
    </div>
  </div>
</dialog>

<style>
  /* Animate the inner panel sliding in from the right when the dialog opens. */
  dialog[open] .absolute {
    animation: slide-in 200ms ease-out both;
  }

  @keyframes slide-in {
    from {
      transform: translateX(100%);
    }
    to {
      transform: translateX(0);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    dialog[open] .absolute {
      animation: none;
    }
  }
</style>

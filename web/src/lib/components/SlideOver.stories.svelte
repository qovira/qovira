<script module lang="ts">
  // SlideOver stories — exercises focus-restore on every close path.
  //
  // Uses SlideOver.fixture.svelte (a thin wrapper that owns the $state open binding)
  // so each story renders a standalone trigger + SlideOver pair without depending
  // on Storybook args reactivity for internal component state.
  //
  // NOTE: <dialog>.showModal() requires a real browser — these stories run under
  // the `storybook` Vitest project (Playwright/chromium), not happy-dom.
  //
  // How focus-restore is isolated from Chromium's native dialog behaviour
  // -----------------------------------------------------------------------
  // A native <dialog> opened with .showModal() MAY restore focus to its
  // "previously-focused element" when .close() is called — but only when the
  // browser itself tracks the invoker (i.e. the dialog was opened by a direct
  // user-gesture on the element it wants to restore to). When the dialog is
  // opened from a $effect (prop → showModal), the browser does NOT reliably
  // record an invoker, so native focus-restore is absent or targets <body>.
  //
  // The component's restoreFocus() explicitly calls previouslyFocused.focus().
  // Chromium's native path does NOT call the element's JS .focus() method — it
  // sets focus directly at the platform level. Installing a spy via spyOn(el, 'focus')
  // therefore captures ONLY the component's programmatic call, not native restoration.
  //
  // Each story:
  //  1. Opens the dialog via the trigger button.
  //  2. Installs a spy on the trigger button's .focus() method AFTER the dialog is open
  //     (so that userEvent.click(trigger) focus events are not counted).
  //  3. Triggers the close path under test.
  //  4. Asserts the spy was called (component's restoreFocus() ran).
  //
  // Revert-matrix verification: gut restoreFocus() in SlideOver.svelte → every
  // spy assertion below goes RED. Restore the implementation → all GREEN again.
  //
  // Acceptance criteria exercised via `play` in chromium:
  //   - Esc/cancel path: spy on trigger.focus() fires (component restores focus).
  //   - Close button:    spy on trigger.focus() fires.
  //   - Programmatic:    spy on trigger.focus() fires ($effect else-branch).
  //   - Double-restore:  spy on trigger.focus() fires exactly ONCE, not twice
  //                      (previouslyFocused cleared after first call = no-op guard).

  import { defineMeta } from "@storybook/addon-svelte-csf";
  import { expect, waitFor, spyOn } from "storybook/test";
  import SlideOverFixture from "./SlideOver.fixture.svelte";

  const { Story } = defineMeta({
    title: "Layout/SlideOver",
    component: SlideOverFixture,
    tags: ["autodocs"],
    parameters: {
      layout: "padded",
    },
  });
</script>

<!--
  EscRestoresFocus: trigger gets focus, open the dialog, dispatch cancel event (Esc path).
  A spy on trigger.focus() installed AFTER the dialog is open asserts the COMPONENT's
  restoreFocus() fired on close — not Chromium's native restoration, which sets focus at
  platform level without calling .focus() on the element.

  Spy is installed after the dialog is open so that userEvent.click(trigger) focus
  events are not counted — only the component's post-close .focus() call is captured.

  Note: userEvent.keyboard("{Escape}") dispatches synthetic keyboard events but does NOT
  reliably fire the native <dialog> `cancel` event in Playwright/Chromium (the browser
  handles Esc for dialogs through its own event chain, separate from synthetic keydown).
  We dispatch the cancel event directly to exercise the handleCancel → handleClose path
  reliably, which is the same code path the browser's Esc key triggers.

  Goes RED when restoreFocus() is gutted to a no-op because the spy is never called.
-->
<Story
  name="EscRestoresFocus"
  play={async ({ canvas, userEvent }) => {
    // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment, @typescript-eslint/no-unsafe-call, @typescript-eslint/no-unsafe-member-access
    const trigger = await canvas.findByRole("button", { name: "Open slide-over" });
    // eslint-disable-next-line @typescript-eslint/no-unsafe-call, @typescript-eslint/no-unsafe-member-access
    await userEvent.click(trigger);

    // Dialog should be open (role="dialog" from native <dialog>).
    // eslint-disable-next-line @typescript-eslint/no-unsafe-call, @typescript-eslint/no-unsafe-member-access
    const dialog = canvas.getByRole("dialog") as HTMLDialogElement;
    await expect(dialog).toBeVisible();

    // Install the spy AFTER the dialog is open so that userEvent.click(trigger)
    // focus events are not counted in the assertion.
    const focusSpy = spyOn(trigger, "focus");

    // Dispatch the native cancel event (fired by browser on Esc key press).
    // This exercises handleCancel → handleClose → restoreFocus() → trigger.focus().
    dialog.dispatchEvent(new Event("cancel", { cancelable: true, bubbles: false }));

    // Wait for Svelte's effect flush + focus restoration to complete.
    // The spy must have been called by the component's restoreFocus().
    await waitFor(() => {
      void expect(focusSpy).toHaveBeenCalled();
    });
  }}
/>

<!--
  CloseButtonRestoresFocus: close-button inside the panel restores focus to trigger.
  Spy on trigger.focus() (installed after dialog is open) asserts the component's
  restoreFocus() fired on the handleClose() path.

  Goes RED when restoreFocus() is gutted to a no-op.
-->
<Story
  name="CloseButtonRestoresFocus"
  play={async ({ canvas, userEvent }) => {
    const trigger = await canvas.findByRole("button", { name: "Open slide-over" });
    await userEvent.click(trigger);

    const dialog = canvas.getByRole("dialog");
    await expect(dialog).toBeVisible();

    // Install spy after the dialog is open.
    const focusSpy = spyOn(trigger, "focus");

    // Click the close button inside the dialog.
    const closeBtn = canvas.getByRole("button", { name: "Close" });
    await userEvent.click(closeBtn);

    // Component's restoreFocus() must have called trigger.focus().
    await waitFor(() => {
      void expect(focusSpy).toHaveBeenCalled();
    });
  }}
/>

<!--
  ProgrammaticCloseRestoresFocus: parent sets open=false directly → the $effect
  else-branch calls restoreFocus() → trigger.focus().
  Proves WU1/M4's programmatic-close focus-restore path: the $effect detects the
  open=false edge and explicitly calls restoreFocus() so keyboard users land back
  on the trigger even when no user gesture closed the dialog.

  Goes RED when restoreFocus() is gutted to a no-op.
-->
<Story
  name="ProgrammaticCloseRestoresFocus"
  args={{ showProgrammaticClose: true }}
  play={async ({ canvas, userEvent }) => {
    const trigger = await canvas.findByRole("button", { name: "Open slide-over" });
    await userEvent.click(trigger);

    const dialog = canvas.getByRole("dialog");
    await expect(dialog).toBeVisible();

    // Install spy after the dialog is open.
    const focusSpy = spyOn(trigger, "focus");

    // Close programmatically via the fixture's "Close programmatically" button.
    // This sets open=false directly, bypassing handleClose(). The $effect
    // else-branch detects dialogEl.open===true, calls .close() then restoreFocus().
    const programmaticBtn = await canvas.findByRole("button", { name: "Close programmatically" });
    await userEvent.click(programmaticBtn);

    // The $effect's restoreFocus() must have called trigger.focus().
    await waitFor(() => {
      void expect(focusSpy).toHaveBeenCalled();
    });
  }}
/>

<!--
  DoubleRestoreIsNoop: cancel event → handleClose → restoreFocus → clears previouslyFocused.
  The $effect else-branch then runs but restoreFocus() is a no-op (previouslyFocused is null
  after the first call). The spy must be called exactly ONCE — not twice.

  Spy is installed after the dialog is open so that click-focus events are not counted.

  Gutting restoreFocus() → spy never called → toHaveBeenCalledTimes(1) fails → RED.
  Removing the previouslyFocused=null guard → spy called twice → also fails → RED.
  Only the correct implementation (call once, clear, guard) → spy called exactly once → GREEN.
-->
<Story
  name="DoubleRestoreIsNoop"
  play={async ({ canvas, userEvent }) => {
    // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment, @typescript-eslint/no-unsafe-call, @typescript-eslint/no-unsafe-member-access
    const trigger = await canvas.findByRole("button", { name: "Open slide-over" });
    // eslint-disable-next-line @typescript-eslint/no-unsafe-call, @typescript-eslint/no-unsafe-member-access
    await userEvent.click(trigger);

    // eslint-disable-next-line @typescript-eslint/no-unsafe-call, @typescript-eslint/no-unsafe-member-access
    const dialog = canvas.getByRole("dialog") as HTMLDialogElement;
    await expect(dialog).toBeVisible();

    // Install spy after the dialog is open so click-focus events are excluded.
    const focusSpy = spyOn(trigger, "focus");

    // Dispatch cancel event (same path as Esc key → handleCancel → handleClose → restoreFocus).
    // restoreFocus() clears previouslyFocused; the subsequent $effect else-branch is a no-op.
    dialog.dispatchEvent(new Event("cancel", { cancelable: true, bubbles: false }));

    // Spy must be called exactly once: by handleClose → restoreFocus().
    // The $effect else-branch also calls restoreFocus() but previouslyFocused is null
    // by then, so the `instanceof HTMLElement` guard prevents a second .focus() call.
    await waitFor(() => {
      void expect(focusSpy).toHaveBeenCalledTimes(1);
    });

    // Dialog must be fully closed.
    await expect(dialog).not.toBeVisible();
  }}
/>

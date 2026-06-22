<script lang="ts">
  import { goto } from "$app/navigation";
  import { Button, Field, Input } from "@qovira/ui";

  import { performLogin } from "$lib/auth/login.js";
  import {
    login_heading,
    login_field_email,
    login_field_password,
    login_submit,
    login_error_unexpected,
  } from "$lib/paraglide/messages.js";

  // ---------------------------------------------------------------------------
  // Form state
  // ---------------------------------------------------------------------------
  let email = $state("");
  let password = $state("");
  let loading = $state(false);
  let errorMessage = $state<string | null>(null);
  let emailError = $state<string | undefined>(undefined);
  let passwordError = $state<string | undefined>(undefined);

  // ---------------------------------------------------------------------------
  // Submit handler
  // ---------------------------------------------------------------------------
  async function handleSubmit(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    loading = true;
    errorMessage = null;
    emailError = undefined;
    passwordError = undefined;

    try {
      const result = await performLogin(email, password);
      if (result.ok) {
        // Navigate home. The root-layout guard lets us through because the
        // session store is now populated.
        await goto("/");
        return;
      }
      emailError = result.fieldErrors?.email;
      passwordError = result.fieldErrors?.password;
      if (result.message !== undefined) errorMessage = result.message;
    } catch {
      // Raw network/parse failure (not a ProblemError) — surface a generic message.
      errorMessage = login_error_unexpected();
    } finally {
      // Always reset so the submit button never sticks disabled.
      loading = false;
    }
  }
</script>

<div class="flex min-h-screen items-center justify-center">
  <form class="flex w-full max-w-sm flex-col gap-4" onsubmit={handleSubmit}>
    <h1 class="text-fg text-xl font-semibold">{login_heading()}</h1>

    {#if errorMessage}
      <p class="text-sm text-red-600" role="alert">{errorMessage}</p>
    {/if}

    <!--
      exactOptionalPropertyTypes: only pass `error` when it is defined.
      Input.value is not $bindable; use `value` + `oninput` instead.
    -->
    <Field label={login_field_email()} {...emailError !== undefined ? { error: emailError } : {}}>
      {#snippet children()}
        <Input
          type="email"
          name="email"
          autocomplete="email"
          value={email}
          oninput={(e: Event & { currentTarget: HTMLInputElement }) => {
            email = e.currentTarget.value;
          }}
          disabled={loading}
          required
        />
      {/snippet}
    </Field>

    <Field label={login_field_password()} {...passwordError !== undefined ? { error: passwordError } : {}}>
      {#snippet children()}
        <Input
          type="password"
          name="password"
          autocomplete="current-password"
          value={password}
          oninput={(e: Event & { currentTarget: HTMLInputElement }) => {
            password = e.currentTarget.value;
          }}
          disabled={loading}
          required
        />
      {/snippet}
    </Field>

    <Button variant="primary" type="submit" {loading}>{login_submit()}</Button>
  </form>
</div>

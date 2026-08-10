# New-Issue Draft Discard Confirmation Design

## Goal

Protect work entered in the TUI's new-issue and new-child-issue forms from accidental loss when the user presses Esc, while keeping cancellation frictionless when the form has not changed.

## Behavior

Each new-issue form records its initial field values when it opens. An ordinary form therefore starts with blank Title, Body, Labels, Owner, and Parent values; a new-child form starts with its Parent value prefilled. The prefilled parent is baseline state, not a user edit.

When Esc is pressed:

- An unchanged form closes immediately.
- A form whose normalized field values differ from its initial values opens a discard-confirmation modal.
- A form with a create request in flight absorbs Esc and remains installed.

The confirmation modal owns keyboard input. `Y` discards the draft and returns to the underlying view. `N` or Esc closes only the confirmation and restores the complete form state, including active field, cursor, scroll position, target, and form generation. Other keys are absorbed.

Normalization applies `strings.TrimSpace` to each field independently while preserving all interior content. Whitespace-only differences do not count as work. Editing a field and restoring its original normalized value makes the form unchanged again.

## Implementation

Generalize the existing dirty-comment confirmation path so it can identify the active draft type and render the appropriate prompt. Extend `inputState` with the new-issue form's initial normalized values, captured by both the ordinary and child constructors. The model-level cancellation request checks saving state first, then draft changes, and otherwise delegates to the existing immediate cancellation path.

Keep the change local to the TUI. It does not alter API requests, persistence, schema, or issue creation semantics.

## Presentation

The new confirmation copy identifies a new-issue draft and uses the established actions: `[Y] Discard` and `[N] Keep editing`. Modal footer hints remain `y discard` and `n/esc keep editing`. The new-issue form remains visible behind the confirmation.

## Tests

Regression tests cover:

- immediate cancellation of unchanged ordinary and child forms;
- confirmation after edits to new-issue fields, including child forms;
- no confirmation for whitespace-only changes or edits restored to baseline;
- Esc absorption while a create request is saving;
- discard confirmation, keep-editing behavior, unrelated-key absorption, and exact form-state preservation;
- modal copy, footer hints, and rendered snapshot behavior;
- continued comment-draft confirmation behavior.

# {{title}}

Status: draft
Full gate: `{{full_gate}}`

## Outcome

{{delivery_outcome_and_selected_slice_strategy}}

## Slice Map

| Slice | Short name | Exact branch | Expected PR title | Report |
| --- | --- | --- | --- | --- |
| Slice 1 | {{slice_name}} | `{{branch}}` | `{{pr_title}}` | `{{report_path}}` |

## Slice 1

{{bounded_outcome_dependencies_and_acceptance_ids}}

### Inputs

{{relative_links_to_existing_spec_and_ticket}}

### Tasks

- T1.1: {{implementation_outcome}}
- T1.2: {{validation_and_report_outcome}}

### Validation

{{exact_focused_and_repository_required_commands_to_run}}

## Integration and Closure

Integrate the listed slices into `dev`, run `{{full_gate}}` on the clean
candidate, and close the approved operational documents after the gate passes.

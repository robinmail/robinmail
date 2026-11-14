// Buttons, inputs and badges are the browser's own elements with classes:
// these directives hold the classes so that every screen gets the same ones.

import { Directive, computed, input } from '@angular/core'

type Variant = 'solid' | 'subtle' | 'outline' | 'ghost' | 'link'
type Color = 'primary' | 'neutral' | 'error' | 'warning' | 'success'
type Size = 'xs' | 'sm' | 'md' | 'lg'

const SIZES: Record<Size, string> = {
  xs: 'gap-1 px-2 py-1 text-xs',
  sm: 'gap-1.5 px-2.5 py-1.5 text-xs',
  md: 'gap-1.5 px-2.5 py-1.5 text-sm',
  lg: 'gap-2 px-3 py-2 text-sm',
}

const LOOKS: Record<Variant, Record<Color, string>> = {
  solid: {
    primary: 'bg-primary text-inverted hover:bg-primary/85',
    neutral: 'bg-inverted text-inverted hover:bg-inverted/85',
    error: 'bg-error text-inverted hover:bg-error/85',
    warning: 'bg-warning text-inverted hover:bg-warning/85',
    success: 'bg-success text-inverted hover:bg-success/85',
  },
  subtle: {
    primary: 'bg-primary/10 text-primary ring ring-inset ring-primary/25 hover:bg-primary/15',
    neutral: 'bg-elevated text-default ring ring-inset ring-accented hover:bg-accented/60',
    error: 'bg-error/10 text-error ring ring-inset ring-error/25 hover:bg-error/15',
    warning: 'bg-warning/10 text-warning ring ring-inset ring-warning/25 hover:bg-warning/15',
    success: 'bg-success/10 text-success ring ring-inset ring-success/25 hover:bg-success/15',
  },
  outline: {
    primary: 'text-primary ring ring-inset ring-primary/50 hover:bg-primary/10',
    neutral: 'bg-default text-default ring ring-inset ring-accented hover:bg-elevated',
    error: 'text-error ring ring-inset ring-error/50 hover:bg-error/10',
    warning: 'text-warning ring ring-inset ring-warning/50 hover:bg-warning/10',
    success: 'text-success ring ring-inset ring-success/50 hover:bg-success/10',
  },
  ghost: {
    primary: 'text-primary hover:bg-primary/10',
    neutral: 'text-default hover:bg-elevated',
    error: 'text-error hover:bg-error/10',
    warning: 'text-warning hover:bg-warning/10',
    success: 'text-success hover:bg-success/10',
  },
  link: {
    primary: 'text-primary hover:text-primary/75 !p-0',
    neutral: 'text-muted hover:text-default !p-0',
    error: 'text-error hover:text-error/75 !p-0',
    warning: 'text-warning !p-0',
    success: 'text-success !p-0',
  },
}

@Directive({
  selector: '[rmBtn]',
  host: { '[class]': 'classes()' },
})
export class Btn {
  readonly variant = input<Variant>('solid')
  readonly color = input<Color>('primary')
  readonly size = input<Size>('md')
  readonly block = input(false)
  protected readonly classes = computed(
    () =>
      `inline-flex shrink-0 items-center rounded-md font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary disabled:cursor-not-allowed disabled:opacity-60 aria-disabled:cursor-not-allowed aria-disabled:opacity-60 ${SIZES[this.size()]} ${LOOKS[this.variant()][this.color()]} ${this.block() ? 'w-full justify-center' : ''}`,
  )
}

@Directive({
  selector: '[rmInput]',
  host: {
    class:
      'w-full rounded-md border-0 bg-default px-2.5 py-1.5 text-sm text-highlighted ring ring-inset ring-accented placeholder:text-dimmed outline-none focus-visible:ring-2 focus-visible:ring-primary disabled:cursor-not-allowed disabled:opacity-60',
  },
})
export class Input {}

const BADGES: Record<Color, string> = {
  primary: 'bg-primary/10 text-primary ring-primary/25',
  neutral: 'bg-elevated text-default ring-accented',
  error: 'bg-error/10 text-error ring-error/25',
  warning: 'bg-warning/10 text-warning ring-warning/25',
  success: 'bg-success/10 text-success ring-success/25',
}

@Directive({
  selector: '[rmBadge]',
  host: { '[class]': 'classes()' },
})
export class Badge {
  readonly color = input<Color>('primary')
  protected readonly classes = computed(
    () => `inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs font-medium ring ring-inset ${BADGES[this.color()]}`,
  )
}

// Classes shared by templates that are not worth a directive.
export const ALERT = 'flex items-start gap-2 rounded-md bg-error/10 px-3 py-2 text-sm text-error ring ring-inset ring-error/25'
export const MENU = 'flex max-h-80 min-w-48 flex-col overflow-y-auto rounded-md bg-default p-1 text-sm shadow-lg ring ring-default'
export const MENU_ITEM =
  'flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-default outline-none hover:bg-elevated focus-visible:bg-elevated aria-disabled:opacity-50'
export const SKELETON = 'animate-pulse rounded-md bg-elevated'
export const LABEL = 'flex flex-col gap-1 text-sm font-medium text-default'
export const CARD = 'rounded-lg bg-default ring ring-default'

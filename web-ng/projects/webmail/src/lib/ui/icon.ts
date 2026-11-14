import { ChangeDetectionStrategy, Component, computed, inject, input } from '@angular/core'
import { DomSanitizer } from '@angular/platform-browser'
import { ICONS, type IconName } from './icons'

// One icon. The markup is a constant of this bundle (icons.ts), which is
// why it may go in as HTML; nothing a server or a message says gets here.
// The size comes from the class at the place of use ("size-4").
@Component({
  selector: 'rm-icon',
  template: `<svg viewBox="0 0 24 24" class="block size-full" aria-hidden="true" [innerHTML]="body()"></svg>`,
  host: { class: 'inline-block shrink-0' },
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class Icon {
  private readonly sanitizer = inject(DomSanitizer)
  readonly name = input.required<IconName | string>()
  protected readonly body = computed(() => this.sanitizer.bypassSecurityTrustHtml((ICONS as Record<string, string>)[this.name()] ?? ''))
}

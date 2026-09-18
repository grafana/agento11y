import { Notice, PageHero, PageShell } from './notices';
import { SettingsLocalGuardsCard, type SettingsLocalGuardsProps } from './settings-local-guards';

interface SecurityViewProps {
  localGuards: SettingsLocalGuardsProps | null;
  configError: string | null;
}

export function SecurityView({ localGuards, configError }: SecurityViewProps) {
  return (
    <PageShell maxWidth={1400}>
      <PageHero title="Security" desc="Local guards" />
      {!localGuards ? (
        configError ? (
          <Notice kind="error" title="Failed to load security settings">
            {configError}
          </Notice>
        ) : (
          <Notice kind="info" title="Loading security…">
            Reading config.env.
          </Notice>
        )
      ) : (
        <>
          {!localGuards.data && !localGuards.error && (
            <Notice kind="info" title="Loading guard packs…">
              Reading local guard rules.
            </Notice>
          )}
          <SettingsLocalGuardsCard {...localGuards} />
        </>
      )}
    </PageShell>
  );
}

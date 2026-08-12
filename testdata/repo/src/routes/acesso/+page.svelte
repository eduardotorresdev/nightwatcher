<script lang="ts">
  // Alvo do culprit de ruido-error-instrumentacao.json.
  // Estes throws são instrumentação deliberada de fluxo de acesso, não defeito:
  // é o caso que a Triagem precisa classificar como ruído.
  import { onMount } from 'svelte';

  onMount(() => {
    try {
      throw new Error('POS-SAIDA');
    } catch (e) {
      console.debug(e);
    }
  });
</script>

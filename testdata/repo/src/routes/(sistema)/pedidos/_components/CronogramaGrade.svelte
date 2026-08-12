<script lang="ts">
  // Alvo do stack trace de defeito-error-new.json:
  //   ReferenceError: Tooltip is not defined
  // O componente é usado no markup mas nunca importado — o import abaixo está
  // comentado de propósito, para que o arquivo case com o defeito real.
  // import Tooltip from '$lib/componentes/Tooltip.svelte';
  import { pedidosPorSemana } from '$lib/requisicoes/revisoes-ready-counter.svelte';

  let { pedidos = [] } = $props();
</script>

<div class="cronograma">
  {#each pedidosPorSemana(pedidos) as semana}
    <Tooltip texto={semana.rotulo}>
      <span>{semana.total}</span>
    </Tooltip>
  {/each}
</div>

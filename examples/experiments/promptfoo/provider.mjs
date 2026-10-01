const answers = new Map([
  ['What is the capital of France?', 'Paris'],
  ['What is 2 + 2?', '5'], // Deliberate failure; the assertion still expects 4.
]);

export default class LocalExampleProvider {
  id() {
    return 'local:deterministic-example';
  }

  async callApi(prompt) {
    return { output: answers.get(prompt) ?? 'unknown' };
  }
}

export function shout(s: string): string {
  return s.toUpperCase() + "!";
}

export function Greeting({ name }: { name: string }) {
  return <p className="greeting">Hello, {shout(name)}</p>;
}

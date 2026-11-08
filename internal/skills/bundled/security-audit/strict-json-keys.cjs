"use strict";

// JSON.parse discards earlier object keys; check them before parsing the document.
function hasDuplicateObjectKeys(contents) {
  const containers = [];
  let stringStart = -1;
  let escaped = false;

  for (let index = 0; index < contents.length; index++) {
    const character = contents[index];
    if (stringStart >= 0) {
      if (escaped) {
        escaped = false;
      } else if (character === "\\") {
        escaped = true;
      } else if (character === '"') {
        const keys = containers[containers.length - 1];
        if (keys) {
          let next = index + 1;
          while (next < contents.length && " \t\r\n".includes(contents[next])) next++;
          if (contents[next] === ":") {
            const key = JSON.parse(contents.slice(stringStart, index + 1)).toLowerCase();
            if (keys.has(key)) return true;
            keys.add(key);
          }
        }
        stringStart = -1;
      }
    } else if (character === '"') {
      stringStart = index;
    } else if (character === "{") {
      containers.push(new Set());
    } else if (character === "[") {
      containers.push(null);
    } else if (character === "}" || character === "]") {
      containers.pop();
    }
  }
  return false;
}

module.exports = { hasDuplicateObjectKeys };

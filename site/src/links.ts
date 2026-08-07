const repo = 'https://github.com/clemsix6/BluePods';

export const links = {
  repo,
  whitepaper: `${repo}/blob/main/docs/WHITEPAPER.md`,
  vision: `${repo}/blob/main/docs/VISION.md`,
  /* Percent-encoded because that is the form LinkedIn issues for the profile. */
  author: 'https://www.linkedin.com/in/cl%C3%A9ment-dreiski/',
};

export const author = {
  name: 'Clément Dreiski',
  role: 'Designed and built BluePods',
  /* Served from public/. Replacing the file is enough to change the portrait. */
  photo: '/author.jpg',
};
